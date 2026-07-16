// Package stream は RTSP カメラ (Tapo C212) を WebRTC に変換して
// Wails WebView へ届けるブリッジ。
//
// go2rtc の internal/ は import できないため、pkg/ 配下のプリミティブのみで
// 配線する (alc-app#120)。go2rtc の pkg API は semver 保証がないので、
// バージョン追従時の修正はこのパッケージに閉じ込める。
package stream

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/rtsp"
	g2webrtc "github.com/AlexxIT/go2rtc/pkg/webrtc"
	pion "github.com/pion/webrtc/v4"
)

// Server は 1 台のカメラを複数の WebRTC 視聴者へ配信する。
// LAN 内完結が要件のため STUN/TURN は使わない (host candidate のみ)。
type Server struct {
	rtspURL string
	api     *pion.API

	mu      sync.Mutex
	prod    *rtsp.Conn
	started bool
	conns   []*g2webrtc.Conn
}

// webrtcPort は WebRTC の固定 UDP ポート (ICE UDP mux)。
// ポートを固定することでファイアウォール設定も 1 ポートで済む。
const webrtcPort = ":8555"

func NewServer(rtspURL string) *Server {
	// ICE 候補は loopback + 物理 LAN (RFC1918) の UDP4 のみに制限する。
	// Tailscale (CGNAT 100.64/10) や IPv6 の候補が混ざると、片方向だけ
	// 到達可能なペアが選択されて「接続済みなのに RTP が届かない」事象を
	// 実測で確認済み (例: 127.0.0.1:8555 → 100.95.51.87 は unreachable)。
	// 同一マシンの WebView へは loopback 経路が最短。
	ips := candidateIPs()
	log.Printf("stream: ice candidate ips: %v", ips)
	api, err := g2webrtc.NewServerAPI("udp", webrtcPort, &g2webrtc.Filters{
		Loopback: true,
		Networks: []string{"udp4"},
		IPs:      ips,
	})
	if err != nil {
		log.Printf("stream: webrtc api init failed: %v", err)
	}
	return &Server{rtspURL: rtspURL, api: api}
}

// candidateIPs は loopback + 物理 LAN と思しき IPv4 アドレスを返す。
// CGNAT 帯 (100.64/10 = Tailscale 等) は除外する。
func candidateIPs() []string {
	cgnat := net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
	ips := []string{"127.0.0.1"}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP.To4()
		if ip == nil || !ip.IsPrivate() || cgnat.Contains(ip) {
			continue
		}
		ips = append(ips, ip.String())
	}
	return ips
}

// SetSource はカメラの RTSP URL を差し替え、既存の producer を破棄する
// (次の WHEP リクエストで新 URL に再接続)。
func (s *Server) SetSource(rtspURL string) {
	s.mu.Lock()
	prod := s.prod
	s.rtspURL = rtspURL
	s.prod = nil
	s.started = false
	s.mu.Unlock()

	if prod != nil {
		_ = prod.Stop()
	}
}

// ServeHTTP は WHEP 互換の SDP 交換 (POST application/sdp offer → 201 answer)。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	configured := s.rtspURL != ""
	s.mu.Unlock()
	if !configured {
		http.Error(w, "RTSP source not configured", http.StatusServiceUnavailable)
		return
	}

	offer, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	answer, err := s.Exchange(string(offer))
	if err != nil {
		log.Printf("stream: exchange failed: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/sdp")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(answer))
}

// Status は producer の状態 (メディア・コーデック・受信バイト数) を JSON で返す。
// 「接続はできたが映像が出ない」切り分け用: Recv が増えていれば RTP は届いている。
func (s *Server) Status(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	prod := s.prod
	s.mu.Unlock()

	s.mu.Lock()
	conns := append([]*g2webrtc.Conn(nil), s.conns...)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	b, err := json.Marshal(map[string]any{"producer": prod, "consumers": conns})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(b)
}

// Exchange はブラウザの offer SDP を受け取り、RTSP producer とトラックを
// 接続した answer SDP を返す。
func (s *Server) Exchange(offer string) (string, error) {
	prod, err := s.producer()
	if err != nil {
		return "", err
	}

	if s.api == nil {
		return "", errors.New("stream: webrtc api not initialized")
	}

	pc, err := s.api.NewPeerConnection(pion.Configuration{})
	if err != nil {
		return "", err
	}

	cons := g2webrtc.NewConn(pc)
	cons.FormatName = "webrtc/whep"
	cons.Mode = core.ModePassiveConsumer
	if err = cons.SetOffer(offer); err != nil {
		_ = pc.Close()
		return "", err
	}

	if !s.bindTracks(prod, cons) {
		_ = pc.Close()
		return "", errors.New("stream: no matching media between camera and viewer")
	}

	answer, err := cons.GetCompleteAnswer(nil, nil)
	if err != nil {
		_ = pc.Close()
		return "", err
	}

	s.mu.Lock()
	s.conns = append(s.conns, cons)
	s.mu.Unlock()

	// 注意: pc.OnConnectionStateChange をここで上書きしてはいけない。
	// go2rtc の NewConn が同フックで connected 時に sender.Start() を
	// 呼んでおり、上書きすると RTP が一切送出されない (実測済み)。
	// 状態変化は go2rtc が Fire するイベントを Listen して受け取る。
	cons.Listen(func(msg any) {
		if state, ok := msg.(pion.PeerConnectionState); ok {
			switch state {
			case pion.PeerConnectionStateDisconnected,
				pion.PeerConnectionStateFailed,
				pion.PeerConnectionStateClosed:
				_ = cons.Stop()
				s.removeConn(cons)
			}
		}
	})

	go s.run(prod)

	return answer, nil
}

func (s *Server) removeConn(cons *g2webrtc.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.conns {
		if c == cons {
			s.conns = append(s.conns[:i], s.conns[i+1:]...)
			return
		}
	}
}

// producer は RTSP 接続を lazy に確立し、生きている間は共有する。
func (s *Server) producer() (*rtsp.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.prod != nil {
		return s.prod, nil
	}

	prod := rtsp.NewClient(s.rtspURL)
	if err := prod.Dial(); err != nil {
		return nil, err
	}
	if err := prod.Describe(); err != nil {
		_ = prod.Close()
		return nil, err
	}

	s.prod = prod
	s.started = false
	return prod, nil
}

// bindTracks は consumer の要求メディアと producer のメディアを突き合わせ、
// 一致したトラックを接続する。1 本でも繋がれば true。
func (s *Server) bindTracks(prod *rtsp.Conn, cons *g2webrtc.Conn) bool {
	bound := false
	for _, consMedia := range cons.GetMedias() {
		for _, prodMedia := range prod.GetMedias() {
			if prodMedia.Direction != core.DirectionRecvonly {
				continue
			}
			prodCodec, consCodec := prodMedia.MatchMedia(consMedia)
			if prodCodec == nil {
				continue
			}
			track, err := prod.GetTrack(prodMedia, prodCodec)
			if err != nil {
				log.Printf("stream: get track: %v", err)
				continue
			}
			if err = cons.AddTrack(consMedia, consCodec, track); err != nil {
				log.Printf("stream: add track: %v", err)
				continue
			}
			log.Printf("stream: bound %s: camera=%s viewer=%s",
				prodMedia.Kind, prodCodec.String(), consCodec.String())
			bound = true
			break
		}
	}
	return bound
}

// run は producer の受信ループを回す (producer につき 1 回だけ)。
// 切断時は共有 producer を破棄して次回の WHEP リクエストで再接続させる。
func (s *Server) run(prod *rtsp.Conn) {
	s.mu.Lock()
	if s.prod != prod || s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()

	if err := prod.Start(); err != nil {
		log.Printf("stream: producer stopped: %v", err)
	}

	s.mu.Lock()
	if s.prod == prod {
		s.prod = nil
	}
	s.mu.Unlock()

	_ = prod.Stop()
}
