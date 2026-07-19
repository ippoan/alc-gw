package whip

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientPublishSuccess(t *testing.T) {
	var gotAuth, gotContentType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)

		w.Header().Set("Location", "/resource/abc123")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("v=0\r\no=- answer\r\n"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/ingest/site1", "site-token")
	answer, err := c.Publish(context.Background(), "v=0\r\no=- offer\r\n")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if answer != "v=0\r\no=- answer\r\n" {
		t.Errorf("answer = %q", answer)
	}
	if gotAuth != "Bearer site-token" {
		t.Errorf("Authorization = %q, want Bearer site-token", gotAuth)
	}
	if gotContentType != "application/sdp" {
		t.Errorf("Content-Type = %q, want application/sdp", gotContentType)
	}
	if gotBody != "v=0\r\no=- offer\r\n" {
		t.Errorf("body = %q", gotBody)
	}
	if want := srv.URL + "/resource/abc123"; c.location != want {
		t.Errorf("location = %q, want %q", c.location, want)
	}
}

func TestClientPublishAbsoluteLocation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://sfu.example.net/resource/xyz")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("answer"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/ingest/site1", "")
	if _, err := c.Publish(context.Background(), "offer"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if want := "https://sfu.example.net/resource/xyz"; c.location != want {
		t.Errorf("location = %q, want %q", c.location, want)
	}
}

func TestClientPublishUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid token", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "bad-token")
	_, err := c.Publish(context.Background(), "offer")
	if err == nil {
		t.Fatal("Publish: want error, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want mention of 401", err)
	}
}

func TestClientPublishServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	if _, err := c.Publish(context.Background(), "offer"); err == nil {
		t.Fatal("Publish: want error, got nil")
	}
}

func TestClientPublishMissingLocation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("answer"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	if _, err := c.Publish(context.Background(), "offer"); err == nil {
		t.Fatal("Publish: want error for missing Location, got nil")
	}
}

func TestClientPublishTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	if _, err := c.Publish(ctx, "offer"); err == nil {
		t.Fatal("Publish: want timeout error, got nil")
	}
}

func TestClientClose(t *testing.T) {
	var deleteCalled bool
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Location", "/resource/abc")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("answer"))
		case http.MethodDelete:
			deleteCalled = true
			gotAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "site-token")
	if _, err := c.Publish(context.Background(), "offer"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !deleteCalled {
		t.Error("DELETE was not sent")
	}
	if gotAuth != "Bearer site-token" {
		t.Errorf("Authorization = %q, want Bearer site-token", gotAuth)
	}
	if c.location != "" {
		t.Errorf("location not cleared after Close: %q", c.location)
	}
}

func TestClientCloseNoop(t *testing.T) {
	// Close before a successful Publish must not attempt any request.
	c := NewClient("http://unused.invalid", "")
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestClientCloseFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Location", "/resource/abc")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("answer"))
		case http.MethodDelete:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	if _, err := c.Publish(context.Background(), "offer"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := c.Close(context.Background()); err == nil {
		t.Fatal("Close: want error, got nil")
	}
}
