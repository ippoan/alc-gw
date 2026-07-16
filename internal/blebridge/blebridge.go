// Package blebridge は ble-medical-gateway 互換の WS サーバー (ws://127.0.0.1:9877)。
//
// 点呼UI (alc-app の useBleGateway) はここに接続して体温計・血圧計の
// 接続状態と測定値を受け取る。実体は CoreS3 の BLE ゲートウェイで、
// CoreS3 の payload は元から ble-medical-gateway 互換 JSON なので無変換で流す:
//
//	GW → alc-app
//	  {"type":"ready","version":"0.1.0"}                          (接続直後)
//	  {"type":"heartbeat","thermo":true,"bp":false}               (機器の接続状態)
//	  {"type":"temperature","value":36.5,"unit":"celsius",...}    (CoreS3 payload そのまま)
//	  {"type":"blood_pressure","systolic":120,"diastolic":80,...} (同上)
//
//	alc-app → GW
//	  {"command":"reset"}  → CoreS3 へ中継 (BLE 再スキャン)
package blebridge

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"

	"alc-gw/internal/uibridge"
)

type Server struct {
	ws *uibridge.Server

	mu         sync.Mutex
	thermo, bp bool
	version    string
}

// New を作る。onCommand は UI からのコマンド (例 "reset") の中継先 (nil 可)。
func New(version string, onCommand func(command string)) *Server {
	s := &Server{version: strings.TrimPrefix(version, "v")}
	s.ws = uibridge.New("ble-bridge",
		func() []any { return []any{s.readyMessage(), s.heartbeatMessage()} },
		func(data []byte) {
			var msg struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(data, &msg); err != nil || msg.Command == "" {
				log.Printf("ble-bridge: 不正なコマンド: %.100s", data)
				return
			}
			if onCommand != nil {
				onCommand(msg.Command)
			}
		})
	return s
}

func (s *Server) ListenAndServe(addr string) error { return s.ws.ListenAndServe(addr) }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.ws.ServeHTTP(w, r) }

// SetDeviceState は体温計・血圧計の BLE 接続状態を更新して heartbeat を配る。
func (s *Server) SetDeviceState(thermo, bp bool) {
	s.mu.Lock()
	s.thermo, s.bp = thermo, bp
	s.mu.Unlock()
	s.ws.Broadcast(s.heartbeatMessage())
}

// Measurement は CoreS3 の測定 payload (temperature / blood_pressure) を
// そのまま全クライアントへ流す。
func (s *Server) Measurement(payload []byte) {
	s.ws.BroadcastRaw(payload)
}

func (s *Server) readyMessage() map[string]any {
	return map[string]any{"type": "ready", "version": s.version}
}

func (s *Server) heartbeatMessage() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{"type": "heartbeat", "thermo": s.thermo, "bp": s.bp}
}
