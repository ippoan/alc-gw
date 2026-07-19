// Package whip は C212 (RTSP) の全景映像を、拠点カメラ用シグナリング
// Durable Object (ippoan/alc-app cf-alc-signaling の CameraSignalingRoom、
// ippoan/alc-app#129) 経由で管理者ブラウザへ P2P WebRTC (STUN のみ) 中継する。
//
// 当初は WHIP (RFC 9725, HTTP POST/DELETE) + クラウド SFU 方式だったが、
// 同時複数視聴が不要なこと・DO ベースのシグナリング資産が既にあることから、
// 「device 役が signaling room へ常時接続し、admin 役が現れた時だけ
// SDP/ICE を交換して P2P を開通させる」方式に転換した (docs/whip-convention.md
// は廃止予定、ippoan/alc-app#129 参照)。P4 ファーム (esp_peer) 版の
// 一次リファレンスという位置付けは変わらない。
package whip

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/gorilla/websocket"
)

type signalingMessage struct {
	Type      string        `json:"type"`
	SDP       string        `json:"sdp,omitempty"`
	Candidate *iceCandidate `json:"candidate,omitempty"`
	Role      string        `json:"role,omitempty"`
	Message   string        `json:"message,omitempty"`
}

type iceCandidate struct {
	Candidate     string  `json:"candidate"`
	SDPMid        *string `json:"sdpMid,omitempty"`
	SDPMLineIndex *int    `json:"sdpMLineIndex,omitempty"`
}

// signalingEvent is what pumpSignaling delivers: either a parsed message
// or the terminal read error (never both).
type signalingEvent struct {
	msg signalingMessage
	err error
}

// dialSignaling opens the persistent device-role WebSocket to the camera
// signaling room. endpoint is the room's WS URL (e.g.
// "wss://alc-signaling.../cam-room/<拠点ID>"); token is sent as a Bearer
// header for forward-compat — the DO does not enforce it yet (v1、
// ippoan/auth-worker#406 完了後に mint 統合予定)。
func dialSignaling(ctx context.Context, endpoint, token string) (*websocket.Conn, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("whip: parse signaling endpoint: %w", err)
	}
	q := u.Query()
	q.Set("role", "device")
	u.RawQuery = q.Encode()

	header := http.Header{}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), header)
	if err != nil {
		return nil, fmt.Errorf("whip: dial signaling: %w", err)
	}
	return conn, nil
}

// sendSignaling writes a message to conn. Best-effort: WriteJSON errors are
// surfaced to the caller, who is expected to treat them the same as a read
// failure (the connection is dead either way).
func sendSignaling(conn *websocket.Conn, msg signalingMessage) error {
	if err := conn.WriteJSON(msg); err != nil {
		return fmt.Errorf("whip: write signaling message: %w", err)
	}
	return nil
}

// pumpSignaling reads JSON messages off conn until it errors, delivering
// each to events. Exactly one event carrying a non-nil err is sent as the
// last event before the channel is closed. Runs until conn is closed by
// the caller (e.g. on ctx cancellation) or the peer drops the connection.
func pumpSignaling(conn *websocket.Conn, events chan<- signalingEvent) {
	defer close(events)
	for {
		var m signalingMessage
		if err := conn.ReadJSON(&m); err != nil {
			events <- signalingEvent{err: err}
			return
		}
		events <- signalingEvent{msg: m}
	}
}
