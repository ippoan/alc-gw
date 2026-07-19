package whip

import (
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

func TestNextBackoff(t *testing.T) {
	cases := []struct {
		cur  time.Duration
		want time.Duration
	}{
		{1 * time.Second, 2 * time.Second},
		{2 * time.Second, 4 * time.Second},
		{32 * time.Second, 60 * time.Second}, // caps below 64s
		{60 * time.Second, 60 * time.Second}, // stays at cap
		{45 * time.Second, 60 * time.Second},
	}
	for _, c := range cases {
		if got := nextBackoff(c.cur); got != c.want {
			t.Errorf("nextBackoff(%s) = %s, want %s", c.cur, got, c.want)
		}
	}
}

func TestFindH264Video(t *testing.T) {
	h264Codec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: 96}
	audioMedia := &core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly,
		Codecs: []*core.Codec{{Name: core.CodecPCMA}}}
	videoMedia := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly,
		Codecs: []*core.Codec{h264Codec}}

	media, codec := findH264Video([]*core.Media{audioMedia, videoMedia})
	if media != videoMedia || codec != h264Codec {
		t.Fatalf("findH264Video = (%v, %v), want (%v, %v)", media, codec, videoMedia, h264Codec)
	}
}

func TestFindH264VideoNoMatch(t *testing.T) {
	audioMedia := &core.Media{Kind: core.KindAudio, Direction: core.DirectionRecvonly,
		Codecs: []*core.Codec{{Name: core.CodecPCMA}}}
	vp8Media := &core.Media{Kind: core.KindVideo, Direction: core.DirectionRecvonly,
		Codecs: []*core.Codec{{Name: core.CodecVP8}}}

	media, codec := findH264Video([]*core.Media{audioMedia, vp8Media})
	if media != nil || codec != nil {
		t.Fatalf("findH264Video = (%v, %v), want (nil, nil)", media, codec)
	}
}

func TestSessionDisabledWithoutWHIPURL(t *testing.T) {
	s := NewSession()
	s.Start(Config{RTSPURL: "rtsp://cam/stream2"}) // WHIPURL 空

	if got := s.Status().State; got != StateDisabled {
		t.Errorf("Status().State = %q, want %q", got, StateDisabled)
	}

	// 起動していないので Stop は何もせず即座に返る。
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop() hung on a Session that was never started")
	}
}

func TestSessionStopBeforeStart(t *testing.T) {
	s := NewSession()
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop() hung on a fresh Session")
	}
}

func TestSessionStartStopLifecycle(t *testing.T) {
	// WHIPURL はダミー (到達しない) が、Start/Stop がハングしないことだけ確認する。
	// runOnce は RTSP dial (存在しないホスト) で早期に失敗し、バックオフ待ちに
	// 入るはずなので、Stop はそのバックオフ待ちを打ち切れる必要がある。
	s := NewSession()
	s.Start(Config{
		RTSPURL: "rtsp://127.0.0.1:1/does-not-exist",
		WHIPURL: "http://127.0.0.1:1/does-not-exist",
	})

	if got := s.Status().State; got == StateDisabled {
		t.Errorf("Status().State = %q, want a non-disabled state", got)
	}

	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return promptly")
	}

	if got := s.Status().State; got != StateStopped {
		t.Errorf("Status().State = %q, want %q", got, StateStopped)
	}
}
