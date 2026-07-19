// Package whip implements a minimal WHIP (WebRTC-HTTP Ingestion Protocol,
// RFC 9725) publisher client: SDP offer/answer exchange over HTTP plus the
// pion PeerConnection wiring needed to forward an RTSP camera's H.264 video
// to a WHIP-compatible SFU, unchanged (no transcode).
//
// docs/whip-convention.md はこのクライアントが前提とするエンドポイント形式・
// 認証・ストリームID規約を定義する。P4 ファーム (esp_peer) 版の一次リファレンス。
package whip

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a single WHIP publish session's signaling channel. It is not
// safe for concurrent Publish calls; a new Client should be created per
// connection attempt (see Session).
type Client struct {
	httpClient *http.Client
	endpoint   string
	token      string

	location string // resource URL from the 201 Location header, for DELETE
}

func NewClient(endpoint, token string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		endpoint:   endpoint,
		token:      token,
	}
}

// Publish POSTs the offer SDP to the WHIP endpoint and returns the answer
// SDP. On success the session's resource Location is recorded for Close.
func (c *Client) Publish(ctx context.Context, offerSDP string) (answerSDP string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(offerSDP))
	if err != nil {
		return "", fmt.Errorf("whip: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/sdp")
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("whip: publish request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("whip: read response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("whip: publish failed: %s: %s", resp.Status, bytes.TrimSpace(body))
	}

	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", errors.New("whip: 201 response missing Location header")
	}
	c.location = resolveLocation(c.endpoint, loc)

	return string(body), nil
}

// Close sends DELETE to the resource URL returned by Publish to tell the
// SFU to tear down the session. A no-op (nil) if Publish never succeeded.
func (c *Client) Close(ctx context.Context) error {
	if c.location == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.location, nil)
	if err != nil {
		return fmt.Errorf("whip: build delete request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("whip: delete request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	c.location = ""

	if resp.StatusCode >= 300 {
		return fmt.Errorf("whip: delete failed: %s", resp.Status)
	}
	return nil
}

func (c *Client) setAuth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// resolveLocation turns a possibly-relative Location header into an
// absolute URL against the WHIP endpoint, per RFC 9725 §4.6.
func resolveLocation(endpoint, location string) string {
	base, err := url.Parse(endpoint)
	if err != nil {
		return location
	}
	ref, err := url.Parse(location)
	if err != nil {
		return location
	}
	return base.ResolveReference(ref).String()
}
