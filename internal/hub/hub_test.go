package hub_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"alc-gw/internal/blebridge"
	"alc-gw/internal/fc1200bridge"
	"alc-gw/internal/hub"
	"alc-gw/internal/nfcbridge"
)

// main.go と同じ配線を組み立てる (CoreS3 → hub → 各ブリッジ → 点呼UI)。
func newWiring() (*hub.Hub, *nfcbridge.Server, *blebridge.Server, *fc1200bridge.Server) {
	nfcBridge := nfcbridge.New("v0.0.0-test")
	var h *hub.Hub
	bleBridge := blebridge.New("v0.0.0-test", func(cmd string) { h.SendCommand("ble_command", cmd) })
	fcBridge := fc1200bridge.New(func(cmd string) { h.SendCommand("fc1200_command", cmd) })
	h = hub.New(hub.Callbacks{
		Devices: func(devices []string) {
			nfcBridge.SetReaders(devices)
			if len(devices) == 0 {
				bleBridge.SetDeviceState(false, false)
			}
		},
		NfcRead: nfcBridge.InjectRead,
		Measurement: func(kind string, payload []byte) {
			switch kind {
			case "temperature", "blood_pressure":
				bleBridge.Measurement(payload)
			case "alcohol":
				fcBridge.Alcohol(payload)
			}
		},
		BleStatus:   bleBridge.SetDeviceState,
		Fc1200Event: fcBridge.Event,
	})
	return h, nfcBridge, bleBridge, fcBridge
}

// CoreS3 → hub → nfcbridge → 点呼UI (alc-app useNfcWebSocket) の中継経路を
// 実 WS 接続で通しで確認する。
func TestCoreS3ToNfcBridgeRelay(t *testing.T) {
	h, nfcBridge, _, _ := newWiring()

	hubSrv := httptest.NewServer(h)
	defer hubSrv.Close()
	bridgeSrv := httptest.NewServer(nfcBridge)
	defer bridgeSrv.Close()

	// 点呼UI 役のクライアント (alc-app が ws://127.0.0.1:9876 に繋ぐのと同じ)
	app := dial(t, bridgeSrv.URL)
	defer app.Close()

	// 接続直後に status (readers 空) が来る
	msg := readJSON(t, app)
	if msg["type"] != "status" {
		t.Fatalf("最初のメッセージが status でない: %v", msg)
	}
	if n := len(msg["readers"].([]any)); n != 0 {
		t.Fatalf("接続前なのに readers が空でない: %v", msg)
	}
	if msg["version"] != "0.0.0-test" {
		t.Fatalf("version の v が剥がれていない: %v", msg)
	}

	// CoreS3 役のクライアントが hub に接続 → readers に載る
	cores3 := dial(t, hubSrv.URL)
	defer cores3.Close()

	msg = readJSON(t, app)
	readers := msg["readers"].([]any)
	if len(readers) != 1 || !strings.HasPrefix(readers[0].(string), "cores3@") {
		t.Fatalf("接続直後の readers が cores3@<addr> でない: %v", msg)
	}

	// hello でデバイス名が確定する
	writeJSON(t, cores3, map[string]any{
		"src": "cores3", "type": "hello", "device": "cores3-01", "fw": "v0.4.0",
	})
	msg = readJSON(t, app)
	readers = msg["readers"].([]any)
	if len(readers) != 1 || readers[0] != "cores3-01" {
		t.Fatalf("hello 後の readers が device 名でない: %v", msg)
	}

	// nfc_read が employee_id に変換されて届く
	writeJSON(t, cores3, map[string]any{
		"src": "cores3", "type": "nfc_read", "card_id": "04A1B2C3D4", "card_type": "mifare",
	})
	msg = readJSON(t, app)
	if msg["type"] != "nfc_read" || msg["employee_id"] != "04A1B2C3D4" {
		t.Fatalf("nfc_read が中継されない: %v", msg)
	}

	// 未知 type と不正 JSON は落ちずに無視される
	writeJSON(t, cores3, map[string]any{"src": "cores3", "type": "future_thing"})
	if err := cores3.WriteMessage(websocket.TextMessage, []byte("not json")); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 切断で readers が空に戻る
	cores3.Close()
	msg = readJSON(t, app)
	if msg["type"] != "status" || len(msg["readers"].([]any)) != 0 {
		t.Fatalf("切断後に readers が空に戻らない: %v", msg)
	}
}

// CoreS3 → hub → blebridge → 点呼UI (alc-app useBleGateway) の体温・血圧中継。
func TestCoreS3ToBleBridgeRelay(t *testing.T) {
	h, _, bleBridge, _ := newWiring()

	hubSrv := httptest.NewServer(h)
	defer hubSrv.Close()
	bleSrv := httptest.NewServer(bleBridge)
	defer bleSrv.Close()

	app := dial(t, bleSrv.URL)
	defer app.Close()

	// 接続直後: ready (version 入り) → heartbeat (機器未接続)
	msg := readJSON(t, app)
	if msg["type"] != "ready" || msg["version"] != "0.0.0-test" {
		t.Fatalf("最初のメッセージが ready でない: %v", msg)
	}
	msg = readJSON(t, app)
	if msg["type"] != "heartbeat" || msg["thermo"] != false || msg["bp"] != false {
		t.Fatalf("heartbeat の初期状態が不正: %v", msg)
	}

	cores3 := dial(t, hubSrv.URL)
	defer cores3.Close()

	// 体温計が BLE 接続された → heartbeat
	writeJSON(t, cores3, map[string]any{
		"src": "cores3", "type": "ble_status", "thermo": true, "bp": false,
	})
	msg = readJSON(t, app)
	if msg["type"] != "heartbeat" || msg["thermo"] != true || msg["bp"] != false {
		t.Fatalf("ble_status が heartbeat に変換されない: %v", msg)
	}

	// 体温の測定 payload (CoreS3 の recorder.rs が組む形) がそのまま届く
	writeJSON(t, cores3, map[string]any{
		"src": "cores3", "type": "measurement", "kind": "temperature",
		"payload": map[string]any{"type": "temperature", "value": 36.5, "unit": "celsius", "measured_at": 20260716120000},
	})
	msg = readJSON(t, app)
	if msg["type"] != "temperature" || msg["value"] != 36.5 {
		t.Fatalf("体温が中継されない: %v", msg)
	}

	// 血圧も同様
	writeJSON(t, cores3, map[string]any{
		"src": "cores3", "type": "measurement", "kind": "blood_pressure",
		"payload": map[string]any{"type": "blood_pressure", "systolic": 120, "diastolic": 80, "pulse": 60, "unit": "mmHg"},
	})
	msg = readJSON(t, app)
	if msg["type"] != "blood_pressure" || msg["systolic"] != float64(120) || msg["pulse"] != float64(60) {
		t.Fatalf("血圧が中継されない: %v", msg)
	}

	// UI からの reset コマンドが CoreS3 へ中継される
	writeJSON(t, app, map[string]any{"command": "reset"})
	msg = readJSON(t, cores3)
	if msg["src"] != "gw" || msg["type"] != "ble_command" || msg["command"] != "reset" {
		t.Fatalf("ble_command が CoreS3 に届かない: %v", msg)
	}

	// CoreS3 全切断 → heartbeat が false/false に戻る
	cores3.Close()
	msg = readJSON(t, app)
	if msg["type"] != "heartbeat" || msg["thermo"] != false || msg["bp"] != false {
		t.Fatalf("切断後に heartbeat がリセットされない: %v", msg)
	}
}

// CoreS3 → hub → fc1200bridge → 点呼UI (alc-app useFc1200Serial) のアルコール中継。
func TestCoreS3ToFc1200BridgeRelay(t *testing.T) {
	h, _, _, fcBridge := newWiring()

	hubSrv := httptest.NewServer(h)
	defer hubSrv.Close()
	fcSrv := httptest.NewServer(fcBridge)
	defer fcSrv.Close()

	app := dial(t, fcSrv.URL)
	defer app.Close()

	// 接続直後: connected
	msg := readJSON(t, app)
	if msg["type"] != "connected" {
		t.Fatalf("最初のメッセージが connected でない: %v", msg)
	}

	cores3 := dial(t, hubSrv.URL)
	defer cores3.Close()

	// alcohol 測定 (CoreS3 の fc1200::payload_json の形) → measurement_result に変換
	writeJSON(t, cores3, map[string]any{
		"src": "cores3", "type": "measurement", "kind": "alcohol",
		"payload": map[string]any{"type": "alcohol", "value": 0.15, "unit": "mg/L", "result": "normal", "use_count": 42},
	})
	msg = readJSON(t, app)
	if msg["type"] != "measurement_result" || msg["alcohol_value"] != 0.15 ||
		msg["result_type"] != "normal" || msg["use_count"] != float64(42) {
		t.Fatalf("alcohol が measurement_result に変換されない: %v", msg)
	}

	// fc1200_event (state_changed 等) はそのまま届く
	writeJSON(t, cores3, map[string]any{
		"src": "cores3", "type": "fc1200_event",
		"payload": map[string]any{"type": "state_changed", "to": "waiting_breath"},
	})
	msg = readJSON(t, app)
	if msg["type"] != "state_changed" || msg["to"] != "waiting_breath" {
		t.Fatalf("fc1200_event が中継されない: %v", msg)
	}

	// UI からのコマンドが CoreS3 へ中継される
	writeJSON(t, app, map[string]any{"command": "sensor_lifetime"})
	msg = readJSON(t, cores3)
	if msg["src"] != "gw" || msg["type"] != "fc1200_command" || msg["command"] != "sensor_lifetime" {
		t.Fatalf("fc1200_command が CoreS3 に届かない: %v", msg)
	}
}

func dial(t *testing.T, httpURL string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(httpURL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	return conn
}

func readJSON(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal %q: %v", data, err)
	}
	return msg
}

func writeJSON(t *testing.T, conn *websocket.Conn, msg map[string]any) {
	t.Helper()
	data, _ := json.Marshal(msg)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}
