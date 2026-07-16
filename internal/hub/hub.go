// Package hub は CoreS3 (ippoan/alc-app-s3) を受ける LAN 内 WS ハブ。
//
// GW がサーバー・CoreS3 がクライアント (CoreS3 にサーバーは立てない = メモリ懸念、
// alc-app#120)。メッセージは JSON テキストフレーム 1 通 = 1 オブジェクト:
//
//	CoreS3 → GW
//	  {"src":"cores3","type":"hello","device":"cores3-01","fw":"v0.4.0"}
//	  {"src":"cores3","type":"nfc_read","card_id":"04A1B2C3D4","card_type":"mifare"}
//
// 未知の type は無視 (ログのみ)。測定値等のメッセージは今後ここに追加する。
// 接続直後は hello 未受信でも "cores3@<addr>" としてデバイス登録する
// (CoreS3 が繋がった時点で NFC リーダーとして見せたいため)。
package hub

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// サーバー → クライアントの keep-alive ping 間隔
	pingInterval = 30 * time.Second
	// pong (または任意のフレーム) が来ないと切断とみなす
	readTimeout = 90 * time.Second
	writeWait   = 5 * time.Second
)

// message は CoreS3 からの全メッセージの和集合。
type message struct {
	Src      string `json:"src"`
	Type     string `json:"type"`
	Device   string `json:"device,omitempty"`
	Fw       string `json:"fw,omitempty"`
	CardID   string `json:"card_id,omitempty"`
	CardType string `json:"card_type,omitempty"`
}

type client struct {
	conn    *websocket.Conn
	name    string
	writeMu sync.Mutex
}

// Hub は接続中の CoreS3 を管理し、イベントをコールバックで通知する。
type Hub struct {
	mu      sync.Mutex
	clients map[*client]struct{}

	// onDevices は接続デバイス一覧の変化時に呼ばれる (名前のリスト)
	onDevices func([]string)
	// onNfcRead は nfc_read 受信時に呼ばれる
	onNfcRead func(cardID string)
}

func New(onDevices func([]string), onNfcRead func(cardID string)) *Hub {
	return &Hub{
		clients:   make(map[*client]struct{}),
		onDevices: onDevices,
		onNfcRead: onNfcRead,
	}
}

var upgrader = websocket.Upgrader{
	// LAN 内の CoreS3 (ブラウザではない) からの接続に Origin は付かない。
	// テスト用にブラウザからの接続も許可する
	CheckOrigin: func(*http.Request) bool { return true },
}

// ListenAndServe は addr (例 ":9000") で WS 接続を待ち受ける。ブロックする。
func (h *Hub) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, h)
}

// ServeHTTP は WS へのアップグレードを受ける (パスは不問)。
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("hub: upgrade: %v", err)
		return
	}
	c := &client{conn: conn, name: "cores3@" + conn.RemoteAddr().String()}

	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	log.Printf("hub: connected: %s", c.name)
	h.notifyDevices()

	go h.pingLoop(c)
	h.readLoop(c)

	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	log.Printf("hub: disconnected: %s", c.name)
	h.notifyDevices()
}

func (h *Hub) readLoop(c *client) {
	defer c.conn.Close()
	_ = c.conn.SetReadDeadline(time.Now().Add(readTimeout))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(readTimeout))
	})
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(readTimeout))

		var msg message
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("hub: %s: 不正な JSON: %.100s", c.name, data)
			continue
		}
		switch msg.Type {
		case "hello":
			if msg.Device != "" {
				h.mu.Lock()
				c.name = msg.Device
				h.mu.Unlock()
			}
			log.Printf("hub: hello: device=%s fw=%s", msg.Device, msg.Fw)
			h.notifyDevices()
		case "nfc_read":
			if msg.CardID == "" {
				log.Printf("hub: %s: nfc_read に card_id がありません", c.name)
				continue
			}
			log.Printf("hub: nfc_read: %s (%s)", msg.CardID, msg.CardType)
			if h.onNfcRead != nil {
				h.onNfcRead(msg.CardID)
			}
		default:
			log.Printf("hub: %s: 未知の type: %q", c.name, msg.Type)
		}
	}
}

func (h *Hub) pingLoop(c *client) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for range t.C {
		c.writeMu.Lock()
		err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait))
		c.writeMu.Unlock()
		if err != nil {
			return
		}
	}
}

// Devices は接続中デバイス名の一覧を返す。
func (h *Hub) Devices() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	names := make([]string, 0, len(h.clients))
	for c := range h.clients {
		names = append(names, c.name)
	}
	return names
}

func (h *Hub) notifyDevices() {
	if h.onDevices != nil {
		h.onDevices(h.Devices())
	}
}

// Status は GET /api/hub/status 用のハンドラ (デバッグ mux に載せる)。
func (h *Hub) Status(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"devices": h.Devices()})
}
