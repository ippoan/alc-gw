package whip

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/rtsp"
	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
)

// whipRTPMTU は WHIP へ送出する H.264 RTP パケットの MTU。go2rtc の
// webrtc consumer と同じ値 (pkg/webrtc/consumer.go)。
const whipRTPMTU = 1200

// iceGatherTimeout は ICE candidate 収集の上限。offer/answer とも
// non-trickle (収集完了を待ってから1通で送る) 前提のため、公衆 STUN が
// 引けない環境で無限に待たないための保険。
const iceGatherTimeout = 5 * time.Second

// pingInterval は signaling WebSocket の keepalive 間隔。Cloudflare の
// Hibernatable WebSockets 自体は idle でも接続を切らないが、経路上の
// 中間ノードでの idle timeout に備えて定期的に打つ。
const pingInterval = 30 * time.Second

const (
	minBackoff = 1 * time.Second
	maxBackoff = 60 * time.Second
)

// defaultICEServers は publish 用の既定 ICE サーバー (公衆 STUN のみ)。
// v1 は TURN を使わない前提 (拠点外から到達可能な admin 側ブラウザとの
// 直接 P2P)。
var defaultICEServers = []pion.ICEServer{
	{URLs: []string{"stun:stun.l.google.com:19302"}},
}

// State は Session の現在の接続状態。
type State string

const (
	StateDisabled     State = "disabled"
	StateConnecting   State = "connecting" // signaling WebSocket 接続中
	StateWaiting      State = "waiting"    // signaling 接続済み、admin 待ち
	StateConnected    State = "connected"  // admin と P2P 確立済み
	StateReconnecting State = "reconnecting"
	StateStopped      State = "stopped"
)

// Config は Session 1 本を駆動するのに必要な値。WHIPURL が空なら
// Session は起動しない (whip_url 未設定 = publish 無効、後方互換)。
//
// WHIPURL は名残りのフィールド名だが、中身は WHIP エンドポイントではなく
// signaling room の WebSocket URL (wss://.../cam-room/<拠点ID>) になった
// (ippoan/alc-app#129)。config.json のキー名 (whip_url) は互換のため
// そのまま維持している。
type Config struct {
	RTSPURL string // C212 の stream2 (360p, 転送用)
	WHIPURL string // wss://<signaling>/cam-room/<拠点ID>
	Token   string // 拠点トークン (Bearer)
}

// Status は /api/status に載せる Session の観測用スナップショット。
type Status struct {
	State          State  `json:"state"`
	LastError      string `json:"last_error,omitempty"`
	ReconnectCount int    `json:"reconnect_count,omitempty"`
	ConnectedSince string `json:"connected_since,omitempty"`
}

// Session は 1 台のカメラの signaling room への接続を常時保ち続ける。
// admin (管理者ブラウザ) が現れるたびに RTSP pull + PeerConnection を
// 立ち上げて P2P で映像を流し、admin が去れば畳んで signaling 接続だけ
// 残す。signaling 自体が切れたら指数バックオフで再接続する。
// 無トランスコード (H.264 RTP パススルー、C212 の stream2 を直接転送)。
type Session struct {
	mu     sync.Mutex
	status Status

	cancel context.CancelFunc
	done   chan struct{}
}

func NewSession() *Session {
	return &Session{status: Status{State: StateDisabled}}
}

// Start は cfg で publish ループを起動する。cfg.WHIPURL が空なら何もしない
// (既存の「whip_url 未設定なら機能無効」動作)。既に起動中なら一旦止めてから
// 新しい cfg で再起動する (設定変更の即時反映、stream.Server.SetSource と同じ設計)。
func (s *Session) Start(cfg Config) {
	s.Stop()

	if cfg.WHIPURL == "" {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	s.mu.Lock()
	s.cancel = cancel
	s.done = done
	s.status = Status{State: StateConnecting}
	s.mu.Unlock()

	go s.loop(ctx, done, cfg)
}

// Stop は実行中の publish ループを止める。起動していなければ何もしない。
func (s *Session) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	done := s.done
	s.cancel = nil
	s.done = nil
	s.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	<-done
}

func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *Session) setStatus(mutate func(*Status)) {
	s.mu.Lock()
	mutate(&s.status)
	s.mu.Unlock()
}

func (s *Session) loop(ctx context.Context, done chan struct{}, cfg Config) {
	defer close(done)

	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			s.setStatus(func(st *Status) { st.State = StateStopped })
			return
		}

		connectedOnce, err := s.runOnce(ctx, cfg)

		if ctx.Err() != nil {
			s.setStatus(func(st *Status) { st.State = StateStopped })
			return
		}

		if connectedOnce {
			// signaling には一度でも繋がっていたので次回はすぐ再試行
			// (relay.c の同種ロジックと同じバックオフリセット方針)
			backoff = minBackoff
		}

		log.Printf("whip: signaling session ended: %v (retry in %s)", err, backoff)
		s.setStatus(func(st *Status) {
			st.State = StateReconnecting
			st.LastError = err.Error()
			st.ReconnectCount++
			st.ConnectedSince = ""
		})

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			s.setStatus(func(st *Status) { st.State = StateStopped })
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// nextBackoff は指数バックオフの次の待ち時間を返す (1s→2s→…→60s 上限)。
func nextBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > maxBackoff {
		next = maxBackoff
	}
	return next
}

// runOnce は signaling WebSocket 1 本のライフサイクルを担う: 接続 →
// admin の出入りに応じて viewer (RTSP+PeerConnection) を開閉するループ →
// 接続が切れるか ctx が閉じたら戻る。connectedOnce は signaling の接続に
// 一度でも成功していれば true (呼び出し側のバックオフリセット判定用)。
func (s *Session) runOnce(ctx context.Context, cfg Config) (connectedOnce bool, err error) {
	conn, err := dialSignaling(ctx, cfg.WHIPURL, cfg.Token)
	if err != nil {
		return false, fmt.Errorf("signaling dial: %w", err)
	}
	defer conn.Close()

	s.setStatus(func(st *Status) {
		st.State = StateWaiting
		st.LastError = ""
		st.ConnectedSince = time.Now().Format(time.RFC3339)
	})

	events := make(chan signalingEvent, 8)
	go pumpSignaling(conn, events)

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	var v *viewer // nil = admin 不在、RTSP/PeerConnection は畳んである

	// viewer が動いている間だけ有効になる select 分岐。viewer==nil の間は
	// これらが nil chan (= 常に受信不可) になるので select が素通りする。
	var failedCh <-chan struct{}
	var prodErrCh <-chan error

	closeViewer := func() {
		if v != nil {
			v.close()
			v = nil
			failedCh = nil
			prodErrCh = nil
		}
	}
	defer closeViewer()

	for {
		select {
		case <-ctx.Done():
			return true, ctx.Err()

		case <-ticker.C:
			if err := sendSignaling(conn, signalingMessage{Type: "ping"}); err != nil {
				return true, err
			}

		case <-failedCh:
			log.Printf("whip: peer connection failed, waiting for admin to reconnect")
			closeViewer()
			s.setStatus(func(st *Status) { st.State = StateWaiting })

		case err := <-prodErrCh:
			if err == nil {
				err = errors.New("rtsp producer stopped")
			}
			log.Printf("whip: rtsp producer stopped: %v", err)
			closeViewer()
			s.setStatus(func(st *Status) { st.State = StateWaiting })

		case ev, ok := <-events:
			if !ok || ev.err != nil {
				if ev.err != nil {
					return true, fmt.Errorf("signaling: read: %w", ev.err)
				}
				return true, errors.New("signaling: connection closed")
			}

			switch ev.msg.Type {
			case "peer_joined":
				if ev.msg.Role == "admin" && v == nil {
					nv, err := startViewer(ctx, conn, cfg)
					if err != nil {
						log.Printf("whip: starting viewer failed: %v", err)
						continue
					}
					v = nv
					failedCh = v.failed
					prodErrCh = v.prodErr
					s.setStatus(func(st *Status) { st.State = StateConnected })
				}
			case "peer_left":
				if ev.msg.Role == "admin" {
					closeViewer()
					s.setStatus(func(st *Status) { st.State = StateWaiting })
				}
			case "sdp_answer":
				if v != nil {
					if err := v.handleAnswer(ev.msg.SDP); err != nil {
						log.Printf("whip: applying sdp_answer failed: %v", err)
						closeViewer()
						s.setStatus(func(st *Status) { st.State = StateWaiting })
					}
				}
			case "ice_candidate":
				// admin (ブラウザ) 側が trickle ICE で送ってきた場合のみ届く。
				// こちら (device) からは送らない (non-trickle、ippoan/alc-app#129)。
				if v != nil && ev.msg.Candidate != nil {
					if err := v.addRemoteCandidate(*ev.msg.Candidate); err != nil {
						log.Printf("whip: add ice candidate failed: %v", err)
					}
				}
			case "error":
				log.Printf("whip: signaling server error: %s", ev.msg.Message)
			}
		}
	}
}

// viewer は 1 admin ぶんの RTSP pull + PeerConnection のライフサイクル。
type viewer struct {
	once sync.Once

	prod        *rtsp.Conn
	pc          *pion.PeerConnection
	senderClose func()

	failed  chan struct{}
	prodErr chan error
}

// startViewer は RTSP に接続し、H.264 を無トランスコードで転送する
// PeerConnection を組んで offer を signaling へ送る。answer は呼び出し側が
// 後続の sdp_answer イベントで handleAnswer に渡す (非同期)。
func startViewer(ctx context.Context, conn *websocket.Conn, cfg Config) (*viewer, error) {
	v := &viewer{
		failed:  make(chan struct{}),
		prodErr: make(chan error, 1),
	}
	ok := false
	defer func() {
		if !ok {
			v.close()
		}
	}()

	prod := rtsp.NewClient(cfg.RTSPURL)
	v.prod = prod
	if err := prod.Dial(); err != nil {
		return nil, fmt.Errorf("rtsp dial: %w", err)
	}
	if err := prod.Describe(); err != nil {
		return nil, fmt.Errorf("rtsp describe: %w", err)
	}

	media, codec := findH264Video(prod.GetMedias())
	if media == nil {
		return nil, errors.New("rtsp source has no H264 video media")
	}

	recvTrack, err := prod.GetTrack(media, codec)
	if err != nil {
		return nil, fmt.Errorf("rtsp get track: %w", err)
	}

	localTrack, err := pion.NewTrackLocalStaticRTP(pion.RTPCodecCapability{
		MimeType:    pion.MimeTypeH264,
		ClockRate:   codec.ClockRate,
		SDPFmtpLine: codec.FmtpLine,
	}, "video", "alc-gw")
	if err != nil {
		return nil, fmt.Errorf("new local track: %w", err)
	}

	pc, err := pion.NewPeerConnection(pion.Configuration{ICEServers: defaultICEServers})
	if err != nil {
		return nil, fmt.Errorf("new peer connection: %w", err)
	}
	v.pc = pc

	rtpSender, err := pc.AddTrack(localTrack)
	if err != nil {
		return nil, fmt.Errorf("add track: %w", err)
	}
	go drainRTCP(rtpSender)

	pc.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		switch state {
		case pion.PeerConnectionStateFailed,
			pion.PeerConnectionStateClosed,
			pion.PeerConnectionStateDisconnected:
			v.once.Do(func() { close(v.failed) })
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return nil, fmt.Errorf("create offer: %w", err)
	}
	gatherComplete := pion.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(offer); err != nil {
		return nil, fmt.Errorf("set local description: %w", err)
	}
	select {
	case <-gatherComplete:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(iceGatherTimeout):
		return nil, errors.New("ice gathering timed out")
	}

	if err := sendSignaling(conn, signalingMessage{Type: "sdp_offer", SDP: pc.LocalDescription().SDP}); err != nil {
		return nil, err
	}

	// 注意: sender.Handler はパケットが流れ始める前 (Start より前) に組む。
	// h264.RTPDepay/RTPPay は go2rtc の webrtc consumer と同じ変換チェーン
	// (pkg/webrtc/consumer.go) — カメラの RTP を 1200 バイト MTU で
	// 再パケタイズしてから TrackLocalStaticRTP へ渡す。無トランスコード。
	sender := core.NewSender(media, codec)
	sender.Handler = func(p *rtp.Packet) { _ = localTrack.WriteRTP(p) }
	if codec.IsRTP() {
		sender.Handler = h264.RTPPay(whipRTPMTU, sender.Handler)
		sender.Handler = h264.RTPDepay(recvTrack.Codec, sender.Handler)
	} else {
		sender.Handler = h264.RTPPay(whipRTPMTU, sender.Handler)
		sender.Handler = h264.RepairAVCC(recvTrack.Codec, sender.Handler)
	}
	sender.Bind(recvTrack)
	sender.Start()
	v.senderClose = sender.Close

	go func() { v.prodErr <- prod.Start() }()

	ok = true
	return v, nil
}

func (v *viewer) handleAnswer(sdp string) error {
	if err := v.pc.SetRemoteDescription(pion.SessionDescription{
		Type: pion.SDPTypeAnswer,
		SDP:  sdp,
	}); err != nil {
		return fmt.Errorf("set remote description: %w", err)
	}
	return nil
}

func (v *viewer) addRemoteCandidate(c iceCandidate) error {
	init := pion.ICECandidateInit{Candidate: c.Candidate}
	if c.SDPMid != nil {
		init.SDPMid = c.SDPMid
	}
	if c.SDPMLineIndex != nil {
		idx := uint16(*c.SDPMLineIndex)
		init.SDPMLineIndex = &idx
	}
	return v.pc.AddICECandidate(init)
}

func (v *viewer) close() {
	v.once.Do(func() { close(v.failed) })
	if v.senderClose != nil {
		v.senderClose()
	}
	if v.prod != nil {
		_ = v.prod.Stop()
	}
	if v.pc != nil {
		_ = v.pc.Close()
	}
}

// drainRTCP は RTCP フィードバックを読み捨てる。RTPSender は誰かが Read
// し続けないと内部バッファが溜まる (pion のお作法)。
func drainRTCP(sender *pion.RTPSender) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buf); err != nil {
			return
		}
	}
}

// findH264Video は producer の media 一覧から H.264 の video (recvonly)
// を探す。v1 スコープは映像のみなので audio は見ない。
func findH264Video(medias []*core.Media) (*core.Media, *core.Codec) {
	for _, media := range medias {
		if media.Kind != core.KindVideo {
			continue
		}
		for _, codec := range media.Codecs {
			if codec.Name == core.CodecH264 {
				return media, codec
			}
		}
	}
	return nil, nil
}
