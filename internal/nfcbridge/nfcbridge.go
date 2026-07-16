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
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const writeWait = 5 * time.Second

type Server struct {
	mu      sync.Mutex
	conns   map[*websocket.Conn]struct{}
	readers []string
	version string
}

// New を version (main.version、"v" 前置あり得る) で作る。
func New(version string) *Server {
	return &Server{
		conns:   make(map[*websocket.Conn]struct{}),
		version: strings.TrimPrefix(version, "v"),
	}
}

var upgrader = websocket.Upgrader{
	// alc.ippoan.org (WebView/ブラウザ) からの loopback 接続を許可する
	CheckOrigin: func(*http.Request) bool { return true },
}

// ListenAndServe は addr (例 "127.0.0.1:9876") で待ち受ける。ブロックする。
func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s)
}

// ServeHTTP は WS へのアップグレードを受ける (パスは不問)。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("nfc-bridge: upgrade: %v", err)
		return
	}

	s.mu.Lock()
	s.conns[conn] = struct{}{}
	// 接続直後に現在の状態を送る (rust-nfc-bridge と同じ振る舞い)
	s.writeLocked(conn, s.statusMessage())
	s.mu.Unlock()
	log.Printf("nfc-bridge: client connected: %s", conn.RemoteAddr())

	// クライアント → サーバー方向のメッセージは無い。切断検知のため読み捨てる
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}

	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
	_ = conn.Close()
	log.Printf("nfc-bridge: client disconnected: %s", conn.RemoteAddr())
}

// SetReaders は NFC リーダー一覧を更新し、全クライアントへ status を配る。
// internal/hub の onDevices に渡す。
func (s *Server) SetReaders(readers []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readers = readers
	s.broadcastLocked(s.statusMessage())
}

// InjectRead はカード読取を全クライアントへ配る。internal/hub の onNfcRead に渡す。
func (s *Server) InjectRead(cardID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broadcastLocked(map[string]any{"type": "nfc_read", "employee_id": cardID})
}

func (s *Server) statusMessage() map[string]any {
	readers := s.readers
	if readers == nil {
		readers = []string{}
	}
	return map[string]any{"type": "status", "readers": readers, "version": s.version}
}

func (s *Server) broadcastLocked(msg map[string]any) {
	for conn := range s.conns {
		s.writeLocked(conn, msg)
	}
}

func (s *Server) writeLocked(conn *websocket.Conn, msg map[string]any) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Printf("nfc-bridge: write: %v", err)
	}
}
