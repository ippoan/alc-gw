package whip

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var testUpgrader = websocket.Upgrader{}

func TestDialSignalingSetsRoleAndAuth(t *testing.T) {
	var gotRole, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRole = r.URL.Query().Get("role")
		gotAuth = r.Header.Get("Authorization")
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("server upgrade: %v", err)
			return
		}
		defer conn.Close()
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/cam-room/site1"
	conn, err := dialSignaling(context.Background(), wsURL, "site-token")
	if err != nil {
		t.Fatalf("dialSignaling: %v", err)
	}
	defer conn.Close()

	if gotRole != "device" {
		t.Errorf("role query param = %q, want %q", gotRole, "device")
	}
	if gotAuth != "Bearer site-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer site-token")
	}
}

func TestDialSignalingNoToken(t *testing.T) {
	var gotAuth string
	var sawAuthHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, sawAuthHeader = r.Header.Get("Authorization"), r.Header.Get("Authorization") != ""
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/cam-room/site1"
	conn, err := dialSignaling(context.Background(), wsURL, "")
	if err != nil {
		t.Fatalf("dialSignaling: %v", err)
	}
	defer conn.Close()

	if sawAuthHeader {
		t.Errorf("Authorization header present without token: %q", gotAuth)
	}
}

func TestPumpSignalingDeliversMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(signalingMessage{Type: "peer_joined", Role: "admin"})
		_ = conn.WriteJSON(signalingMessage{Type: "sdp_answer", SDP: "v=0\r\n..."})
		// Give the client time to read both before we close.
		time.Sleep(50 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/cam-room/site1"
	conn, err := dialSignaling(context.Background(), wsURL, "")
	if err != nil {
		t.Fatalf("dialSignaling: %v", err)
	}
	defer conn.Close()

	events := make(chan signalingEvent, 8)
	go pumpSignaling(conn, events)

	first := recvEvent(t, events)
	if first.err != nil || first.msg.Type != "peer_joined" || first.msg.Role != "admin" {
		t.Fatalf("first event = %+v", first)
	}
	second := recvEvent(t, events)
	if second.err != nil || second.msg.Type != "sdp_answer" || second.msg.SDP != "v=0\r\n..." {
		t.Fatalf("second event = %+v", second)
	}

	// Server closes after the sleep above; pump should surface a terminal
	// error event and then close the channel.
	third := recvEvent(t, events)
	if third.err == nil {
		t.Fatalf("expected terminal error event, got %+v", third)
	}
	if _, ok := <-events; ok {
		t.Fatal("events channel not closed after terminal error")
	}
}

func TestSendSignalingRoundTrip(t *testing.T) {
	received := make(chan signalingMessage, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var m signalingMessage
		if err := conn.ReadJSON(&m); err == nil {
			received <- m
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/cam-room/site1"
	conn, err := dialSignaling(context.Background(), wsURL, "")
	if err != nil {
		t.Fatalf("dialSignaling: %v", err)
	}
	defer conn.Close()

	if err := sendSignaling(conn, signalingMessage{Type: "sdp_offer", SDP: "offer-sdp"}); err != nil {
		t.Fatalf("sendSignaling: %v", err)
	}

	select {
	case m := <-received:
		if m.Type != "sdp_offer" || m.SDP != "offer-sdp" {
			t.Errorf("server received = %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("server never received the message")
	}
}

func recvEvent(t *testing.T, events <-chan signalingEvent) signalingEvent {
	t.Helper()
	select {
	case ev := <-events:
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for signaling event")
		return signalingEvent{}
	}
}
