// Package fc1200bridge は FC-1200 ブリッジ互換の WS サーバー (ws://127.0.0.1:9878)。
//
// 点呼UI (alc-app の useFc1200Serial、WebSocket transport) はここに接続して
// アルコール測定の結果とデバイスイベントを受け取る。FC-1200 実機は CoreS3 の
// USB シリアルに直収されており、プロトコル処理 (fc1200-wasm 相当) も CoreS3
// 側で完結する。GW は結果を alc-app の Fc1200Event 形式へ変換して流すだけ:
//
//	GW → alc-app
//	  {"type":"connected"}                                             (接続直後)
//	  {"type":"measurement_result","alcohol_value":0.15,
//	   "result_type":"normal","use_count":42}                          (kind=alcohol の変換)
//	  {"type":"state_changed","to":"..."} 等                           (fc1200_event そのまま)
//
//	alc-app → GW
//	  {"command":"reset"|"sensor_lifetime"|"memory_read"|...}  → CoreS3 へ中継
package fc1200bridge

import (
	"encoding/json"
	"log"
	"net/http"

	"alc-gw/internal/uibridge"
)

type Server struct {
	ws *uibridge.Server
}

// New を作る。onCommand は UI からのコマンド (例 "reset") の中継先 (nil 可)。
func New(onCommand func(command string)) *Server {
	s := &Server{}
	s.ws = uibridge.New("fc1200-bridge",
		func() []any { return []any{map[string]any{"type": "connected"}} },
		func(data []byte) {
			var msg struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(data, &msg); err != nil || msg.Command == "" {
				log.Printf("fc1200-bridge: 不正なコマンド: %.100s", data)
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

// Alcohol は CoreS3 の alcohol payload
// ({"type":"alcohol","value":0.15,"unit":"mg/L","result":"normal","use_count":42})
// を alc-app の measurement_result イベントに変換して配る。
func (s *Server) Alcohol(payload []byte) {
	var p struct {
		Value    float64 `json:"value"`
		Result   string  `json:"result"`
		UseCount uint32  `json:"use_count"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		log.Printf("fc1200-bridge: alcohol payload の解析失敗: %.100s", payload)
		return
	}
	s.ws.Broadcast(map[string]any{
		"type":          "measurement_result",
		"alcohol_value": p.Value,
		"result_type":   p.Result,
		"use_count":     p.UseCount,
	})
}

// Event は CoreS3 からの Fc1200Event 互換 JSON (state_changed / error 等) を
// そのまま全クライアントへ流す。
func (s *Server) Event(payload []byte) {
	s.ws.BroadcastRaw(payload)
}
