package hub_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"alc-gw/internal/hub"
	"alc-gw/internal/nfcbridge"
)

// CoreS3 → hub → nfcbridge → 点呼UI (alc-app useNfcWebSocket) の中継経路を
// 実 WS 接続で通しで確認する。
func TestCoreS3ToNfcBridgeRelay(t *testing.T) {
	bridge := nfcbridge.New("v0.0.0-test")
	h := hub.New(bridge.SetReaders, bridge.InjectRead)

	hubSrv := httptest.NewServer(h)
	defer hubSrv.Close()
	bridgeSrv := httptest.NewServer(bridge)
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
