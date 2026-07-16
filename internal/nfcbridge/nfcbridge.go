// Package nfcbridge は rust-nfc-bridge 互換の WS サーバー (ws://127.0.0.1:9876)。
//
// WebView 内の点呼UI (alc-app の useNfcWebSocket) はこのポートに接続して
// NFC の状態と読取イベントを受け取る。実体の読取は CoreS3 側 (internal/hub
// 経由) で行い、ここは alc-app が既に話せるプロトコルへ変換するだけ:
//
//	GW → alc-app
//	  {"type":"status","readers":["cores3-01"],"version":"0.1.0"}
//	  {"type":"nfc_read","employee_id":"04A1B2C3D4"}
//
// readers が空でなくなると alc-app の表示が「NFC リーダー未検出」→「NFC 待機中」
// に変わる。rust-nfc-bridge が既に 9876 を掴んでいる場合は Listen に失敗する
// ので、そのままログだけ出して本物に譲る (呼び出し側で continue)。
package nfcbridge

import (
	"net/http"
	"strings"
	"sync"

	"alc-gw/internal/uibridge"
)

type Server struct {
	ws *uibridge.Server

	mu      sync.Mutex
	readers []string
	version string
}

// New を version (main.version、"v" 前置あり得る) で作る。
func New(version string) *Server {
	s := &Server{version: strings.TrimPrefix(version, "v")}
	// 接続直後に現在の状態を送る (rust-nfc-bridge と同じ振る舞い)
	s.ws = uibridge.New("nfc-bridge", func() []any { return []any{s.statusMessage()} }, nil)
	return s
}

func (s *Server) ListenAndServe(addr string) error { return s.ws.ListenAndServe(addr) }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.ws.ServeHTTP(w, r) }

// SetReaders は NFC リーダー一覧を更新し、全クライアントへ status を配る。
// internal/hub の Devices コールバックに渡す。
func (s *Server) SetReaders(readers []string) {
	s.mu.Lock()
	s.readers = readers
	s.mu.Unlock()
	s.ws.Broadcast(s.statusMessage())
}

// InjectRead はカード読取を全クライアントへ配る。internal/hub の NfcRead に渡す。
func (s *Server) InjectRead(cardID string) {
	s.ws.Broadcast(map[string]any{"type": "nfc_read", "employee_id": cardID})
}

func (s *Server) statusMessage() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	readers := s.readers
	if readers == nil {
		readers = []string{}
	}
	return map[string]any{"type": "status", "readers": readers, "version": s.version}
}
