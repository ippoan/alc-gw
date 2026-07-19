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
	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
)

// whipRTPMTU は WHIP へ送出する H.264 RTP パケットの MTU。go2rtc の
// webrtc consumer と同じ値 (pkg/webrtc/consumer.go)。
const whipRTPMTU = 1200

// iceGatherTimeout は ICE candidate 収集の上限。LAN 外向けの非 trickle
// gathering なので、公衆 STUN が引けない環境で無限に待たないための保険。
const iceGatherTimeout = 5 * time.Second

const (
	minBackoff = 1 * time.Second
	maxBackoff = 60 * time.Second
)

// defaultICEServers は WHIP publish 用の既定 ICE サーバー (公衆 STUN のみ)。
// v1 は TURN を使わない前提 (SFU は公衆到達可能なサーバー)。
var defaultICEServers = []pion.ICEServer{
	{URLs: []string{"stun:stun.l.google.com:19302"}},
}

// State は Session の現在の接続状態。
type State string

const (
	StateDisabled     State = "disabled"
	StateConnecting   State = "connecting"
	StateConnected    State = "connected"
	StateReconnecting State = "reconnecting"
	StateStopped      State = "stopped"
)

// Config は Session 1 本を駆動するのに必要な値。WHIPURL が空なら
// Session は起動しない (whip_url 未設定 = publish 無効、後方互換)。
type Config struct {
	RTSPURL string // C212 の stream2 (360p, 転送用)
	WHIPURL string // https://<sfu>/ingest/<拠点ID>
	Token   string // 拠点トークン (Bearer)
}

// Status は /api/status に載せる Session の観測用スナップショット。
type Status struct {
	State          State  `json:"state"`
	LastError      string `json:"last_error,omitempty"`
	ReconnectCount int    `json:"reconnect_count,omitempty"`
	ConnectedSince string `json:"connected_since,omitempty"`
}

// Session は 1 台のカメラを WHIP エンドポイントへ常時 publish し続ける。
// 接続が切れたら指数バックオフで再接続する。無トランスコード
// (H.264 RTP パススルー、C212 の stream2 を直接 WHIP へ転送)。
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

// Stop は実行中の publish ループを止め、SFU 側のリソースを DELETE する。
// 起動していなければ何もしない。
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

		err := s.runOnce(ctx, cfg)

		if ctx.Err() != nil {
			s.setStatus(func(st *Status) { st.State = StateStopped })
			return
		}

		log.Printf("whip: session ended: %v (retry in %s)", err, backoff)
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

// attempt は 1 回の接続試行で確保したリソースをまとめて破棄するための
// ヘルパー。ctx キャンセル時と runOnce の正常/異常終了時のどちらからも
// 呼ばれ得るので sync.Once で二重クローズを防ぐ。
type attempt struct {
	once sync.Once
	prod *rtsp.Conn
	pc   *pion.PeerConnection
	whip *Client
}

func (a *attempt) abort() {
	a.once.Do(func() {
		if a.prod != nil {
			_ = a.prod.Stop()
		}
		if a.pc != nil {
			_ = a.pc.Close()
		}
		if a.whip != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = a.whip.Close(ctx)
			cancel()
		}
	})
}

// runOnce は RTSP 接続 1 回・WHIP publish 1 回のライフサイクルを担う。
// producer が切れる・PeerConnection が failed になる・ctx が閉じる、
// いずれかで戻る (エラーは ctx キャンセル時のみ nil 相当の ctx.Err())。
func (s *Session) runOnce(ctx context.Context, cfg Config) error {
	a := &attempt{}
	watcherDone := make(chan struct{})
	defer close(watcherDone)
	go func() {
		select {
		case <-ctx.Done():
			a.abort()
		case <-watcherDone:
		}
	}()
	defer a.abort()

	prod := rtsp.NewClient(cfg.RTSPURL)
	a.prod = prod
	if err := prod.Dial(); err != nil {
		return fmt.Errorf("rtsp dial: %w", err)
	}
	if err := prod.Describe(); err != nil {
		return fmt.Errorf("rtsp describe: %w", err)
	}

	media, codec := findH264Video(prod.GetMedias())
	if media == nil {
		return errors.New("rtsp source has no H264 video media")
	}

	recvTrack, err := prod.GetTrack(media, codec)
	if err != nil {
		return fmt.Errorf("rtsp get track: %w", err)
	}

	localTrack, err := pion.NewTrackLocalStaticRTP(pion.RTPCodecCapability{
		MimeType:    pion.MimeTypeH264,
		ClockRate:   codec.ClockRate,
		SDPFmtpLine: codec.FmtpLine,
	}, "video", "alc-gw")
	if err != nil {
		return fmt.Errorf("new local track: %w", err)
	}

	pc, err := pion.NewPeerConnection(pion.Configuration{ICEServers: defaultICEServers})
	if err != nil {
		return fmt.Errorf("new peer connection: %w", err)
	}
	a.pc = pc

	rtpSender, err := pc.AddTrack(localTrack)
	if err != nil {
		return fmt.Errorf("add track: %w", err)
	}
	go drainRTCP(rtpSender)

	failed := make(chan struct{})
	var failOnce sync.Once
	pc.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		switch state {
		case pion.PeerConnectionStateConnected:
			s.setStatus(func(st *Status) {
				st.State = StateConnected
				st.LastError = ""
				st.ConnectedSince = time.Now().Format(time.RFC3339)
			})
		case pion.PeerConnectionStateFailed,
			pion.PeerConnectionStateClosed,
			pion.PeerConnectionStateDisconnected:
			failOnce.Do(func() { close(failed) })
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	gatherComplete := pion.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}
	select {
	case <-gatherComplete:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(iceGatherTimeout):
		return errors.New("ice gathering timed out")
	}

	whipClient := NewClient(cfg.WHIPURL, cfg.Token)
	a.whip = whipClient
	answerSDP, err := whipClient.Publish(ctx, pc.LocalDescription().SDP)
	if err != nil {
		return err
	}
	if err = pc.SetRemoteDescription(pion.SessionDescription{
		Type: pion.SDPTypeAnswer,
		SDP:  answerSDP,
	}); err != nil {
		return fmt.Errorf("set remote description: %w", err)
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
	defer sender.Close()

	prodErr := make(chan error, 1)
	go func() { prodErr <- prod.Start() }()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-failed:
		return errors.New("peer connection failed")
	case err := <-prodErr:
		if err == nil {
			err = errors.New("rtsp producer stopped")
		}
		return fmt.Errorf("rtsp producer stopped: %w", err)
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
