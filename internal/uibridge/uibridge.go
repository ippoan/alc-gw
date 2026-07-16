// Package uibridge は点呼UI (alc-app) 向けブリッジ WS サーバーの共通部。
//
// alc-app は Android タブレット時代のブリッジ互換で ws://127.0.0.1:{9876,9877,9878}
// に接続してくる (NFC / BLE 医療機器 / FC-1200)。各ブリッジのプロトコル差は
// greet (接続直後に送るメッセージ) と onMessage (UI からのコマンド) で吸収し、
// 接続管理・ブロードキャストをここに集約する。
package uibridge

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const writeWait = 5 * time.Second

type Server struct {
	name string
	mu   sync.Mutex
	conns map[*websocket.Conn]struct{}

	// greet は接続直後にそのクライアントへ送るメッセージ列 (nil 可)
	greet func() []any
	// onMessage は UI クライアントからの受信 (nil = 読み捨て)
	onMessage func(data []byte)
}

// New を作る。name はログ用 (例 "nfc-bridge")。
func New(name string, greet func() []any, onMessage func(data []byte)) *Server {
	return &Server{
		name:      name,
		conns:     make(map[*websocket.Conn]struct{}),
		greet:     greet,
		onMessage: onMessage,
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
		log.Printf("%s: upgrade: %v", s.name, err)
		return
	}

	s.mu.Lock()
	s.conns[conn] = struct{}{}
	if s.greet != nil {
		for _, msg := range s.greet() {
			s.writeLocked(conn, msg)
		}
	}
	s.mu.Unlock()
	log.Printf("%s: client connected: %s", s.name, conn.RemoteAddr())

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if s.onMessage != nil {
			s.onMessage(data)
		}
	}

	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
	_ = conn.Close()
	log.Printf("%s: client disconnected: %s", s.name, conn.RemoteAddr())
}

// Broadcast は msg を JSON にして全クライアントへ送る。
func (s *Server) Broadcast(msg any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for conn := range s.conns {
		s.writeLocked(conn, msg)
	}
}

// BroadcastRaw は既に JSON になっているバイト列をそのまま全クライアントへ送る
// (CoreS3 の payload を無変換で流す用)。
func (s *Server) BroadcastRaw(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for conn := range s.conns {
		s.writeRawLocked(conn, data)
	}
}

func (s *Server) writeLocked(conn *websocket.Conn, msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	s.writeRawLocked(conn, data)
}

func (s *Server) writeRawLocked(conn *websocket.Conn, data []byte) {
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Printf("%s: write: %v", s.name, err)
	}
}
