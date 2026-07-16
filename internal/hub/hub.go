// Package hub は CoreS3 (ippoan/alc-app-s3) を受ける LAN 内 WS ハブ。
//
// GW がサーバー・CoreS3 がクライアント (CoreS3 にサーバーは立てない = メモリ懸念、
// alc-app#120)。メッセージは JSON テキストフレーム 1 通 = 1 オブジェクト:
//
//	CoreS3 → GW
//	  {"src":"cores3","type":"hello","device":"cores3-01","fw":"v0.4.0"}
//	  {"src":"cores3","type":"nfc_read","card_id":"04A1B2C3D4","card_type":"mifare"}
//	  {"src":"cores3","type":"measurement","kind":"temperature","payload":{...}}
//	      kind: temperature / blood_pressure / alcohol。payload は CoreS3 が
//	      cf-alc-recorder へ送るものと同じ ble-medical-gateway 互換 JSON
//	  {"src":"cores3","type":"ble_status","thermo":true,"bp":false}
//	      体温計・血圧計の BLE 接続状態の変化
//	  {"src":"cores3","type":"fc1200_event","payload":{...}}
//	      FC-1200 の測定結果以外のイベント (alc-app の Fc1200Event 互換 JSON)
//
//	GW → CoreS3
//	  {"src":"gw","type":"ble_command","command":"reset"}
//	  {"src":"gw","type":"fc1200_command","command":"reset"}
//	      command は alc-app の Android ブリッジ互換
//	      (reset / sensor_lifetime / memory_read / memory_complete / date_update:<dt> / connect)
//
// 未知の type は無視 (ログのみ)。接続直後は hello 未受信でも "cores3@<addr>"
// としてデバイス登録する (CoreS3 が繋がった時点で NFC リーダーとして見せたいため)。
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
	Src      string          `json:"src"`
	Type     string          `json:"type"`
	Device   string          `json:"device,omitempty"`
	Fw       string          `json:"fw,omitempty"`
	CardID   string          `json:"card_id,omitempty"`
	CardType string          `json:"card_type,omitempty"`
	Kind     string          `json:"kind,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	Thermo   bool            `json:"thermo,omitempty"`
	Bp       bool            `json:"bp,omitempty"`
}

// Callbacks は CoreS3 からのイベントの通知先 (それぞれ nil 可)。
type Callbacks struct {
	// Devices は接続デバイス一覧の変化時に呼ばれる (名前のリスト)
	Devices func([]string)
	// NfcRead は nfc_read 受信時に呼ばれる
	NfcRead func(cardID string)
	// Measurement は measurement 受信時に kind と payload (JSON) で呼ばれる
	Measurement func(kind string, payload []byte)
	// BleStatus は ble_status 受信時に呼ばれる
	BleStatus func(thermo, bp bool)
	// Fc1200Event は fc1200_event 受信時に payload (JSON) で呼ばれる
	Fc1200Event func(payload []byte)
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
	cb      Callbacks
}

func New(cb Callbacks) *Hub {
	return &Hub{
		clients: make(map[*client]struct{}),
		cb:      cb,
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
		h.handleMessage(c, data)
	}
}

func (h *Hub) handleMessage(c *client, data []byte) {
	var msg message
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("hub: %s: 不正な JSON: %.100s", c.name, data)
		return
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
			return
		}
		log.Printf("hub: nfc_read: %s (%s)", msg.CardID, msg.CardType)
		if h.cb.NfcRead != nil {
			h.cb.NfcRead(msg.CardID)
		}
	case "measurement":
		if msg.Kind == "" || len(msg.Payload) == 0 {
			log.Printf("hub: %s: measurement に kind/payload がありません", c.name)
			return
		}
		log.Printf("hub: measurement: kind=%s %.200s", msg.Kind, msg.Payload)
		if h.cb.Measurement != nil {
			h.cb.Measurement(msg.Kind, msg.Payload)
		}
	case "ble_status":
		log.Printf("hub: ble_status: thermo=%t bp=%t", msg.Thermo, msg.Bp)
		if h.cb.BleStatus != nil {
			h.cb.BleStatus(msg.Thermo, msg.Bp)
		}
	case "fc1200_event":
		if len(msg.Payload) == 0 {
			log.Printf("hub: %s: fc1200_event に payload がありません", c.name)
			return
		}
		log.Printf("hub: fc1200_event: %.200s", msg.Payload)
		if h.cb.Fc1200Event != nil {
			h.cb.Fc1200Event(msg.Payload)
		}
	default:
		log.Printf("hub: %s: 未知の type: %q", c.name, msg.Type)
	}
}

// SendCommand は接続中の全 CoreS3 へ GW からのコマンドを送る。
// typ は "ble_command" / "fc1200_command"。
func (h *Hub) SendCommand(typ, command string) {
	data, err := json.Marshal(map[string]string{"src": "gw", "type": typ, "command": command})
	if err != nil {
		return
	}
	h.mu.Lock()
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()
	for _, c := range clients {
		c.writeMu.Lock()
		_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
		if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
			log.Printf("hub: %s: write: %v", c.name, err)
		}
		c.writeMu.Unlock()
	}
	log.Printf("hub: sent %s: %s", typ, command)
}

// Inject は CoreS3 からのメッセージ受信を装って処理する (デバッグ用。
// POST /api/hub/inject から呼ばれる)。
func (h *Hub) Inject(data []byte) {
	h.handleMessage(&client{name: "inject"}, data)
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
	if h.cb.Devices != nil {
		h.cb.Devices(h.Devices())
	}
}

// Status は GET /api/hub/status 用のハンドラ (デバッグ mux に載せる)。
func (h *Hub) Status(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"devices": h.Devices()})
}
