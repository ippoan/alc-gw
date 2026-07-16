// Package stream は RTSP カメラ (Tapo C212) を WebRTC に変換して
// Wails WebView へ届けるブリッジ。
//
// go2rtc の internal/ は import できないため、pkg/ 配下のプリミティブのみで
// 配線する (alc-app#120)。go2rtc の pkg API は semver 保証がないので、
// バージョン追従時の修正はこのパッケージに閉じ込める。
package stream

import (
	"errors"
	"io"
	"log"
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

	mu      sync.Mutex
	prod    *rtsp.Conn
	started bool
}

func NewServer(rtspURL string) *Server {
	return &Server{rtspURL: rtspURL}
}

// ServeHTTP は WHEP 互換の SDP 交換 (POST application/sdp offer → 201 answer)。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.rtspURL == "" {
		http.Error(w, "RTSP source not configured (ALC_GW_RTSP_URL)", http.StatusServiceUnavailable)
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

// Exchange はブラウザの offer SDP を受け取り、RTSP producer とトラックを
// 接続した answer SDP を返す。
func (s *Server) Exchange(offer string) (string, error) {
	prod, err := s.producer()
	if err != nil {
		return "", err
	}

	api, err := g2webrtc.NewAPI()
	if err != nil {
		return "", err
	}

	pc, err := api.NewPeerConnection(pion.Configuration{})
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

	pc.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		switch state {
		case pion.PeerConnectionStateFailed, pion.PeerConnectionStateClosed:
			_ = cons.Stop()
		}
	})

	go s.run(prod)

	return answer, nil
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
