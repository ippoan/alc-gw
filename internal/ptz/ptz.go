// Package ptz は ONVIF PTZ (パンチルト) でカメラを動かす。
// Tapo C212 はカメラアカウント有効化で ONVIF (ポート 2020) が開き、
// RTSP と同じ認証情報がそのまま使える。
// SOAP の組み立て・WS-Security 認証は go2rtc の pkg/onvif を流用する。
package ptz

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"

	"github.com/AlexxIT/go2rtc/pkg/onvif"
)

const onvifPort = "2020"

type Controller struct {
	onvifURL string

	mu     sync.Mutex
	client *onvif.Client
	ptzURL string
	token  string
}

// FromRTSP は RTSP URL (rtsp://user:pass@host:554/stream1) から
// 認証情報とホストを引き継いで ONVIF 接続先を導出する。
func FromRTSP(rtspURL string) *Controller {
	c := &Controller{}
	c.SetSource(rtspURL)
	return c
}

// SetSource は RTSP URL から ONVIF 接続先を再導出し、既存接続を破棄する。
func (c *Controller) SetSource(rtspURL string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reset()
	c.onvifURL = ""

	u, err := url.Parse(rtspURL)
	if err != nil || u.Hostname() == "" {
		return
	}
	var userinfo string
	if u.User != nil {
		userinfo = u.User.String() + "@"
	}
	c.onvifURL = fmt.Sprintf("http://%s%s:%s/onvif/device_service", userinfo, u.Hostname(), onvifPort)
}

// init はカメラへの ONVIF 接続を lazy に確立する (呼び出し側で mu を保持)。
func (c *Controller) initLocked() error {
	if c.client != nil {
		return nil
	}
	if c.onvifURL == "" {
		return errors.New("ptz: camera not configured")
	}

	client, err := onvif.NewClient(c.onvifURL)
	if err != nil {
		return fmt.Errorf("ptz: onvif connect: %w", err)
	}

	b, err := client.DeviceRequest(onvif.DeviceGetCapabilities)
	if err != nil {
		return fmt.Errorf("ptz: get capabilities: %w", err)
	}
	xaddr := onvif.FindTagValue(b, `PTZ.+?XAddr`)
	if xaddr == "" {
		return errors.New("ptz: camera has no PTZ service")
	}

	u, _ := url.Parse(c.onvifURL)
	ptzURL := "http://" + u.Host + onvif.GetPath(xaddr, "/onvif/ptz_service")

	tokens, err := client.GetProfilesTokens()
	if err != nil {
		return fmt.Errorf("ptz: get profiles: %w", err)
	}
	if len(tokens) == 0 {
		return errors.New("ptz: no media profiles")
	}

	c.client = client
	c.ptzURL = ptzURL
	c.token = tokens[0]
	log.Printf("ptz: ready url=%s profile=%s", ptzURL, c.token)
	return nil
}

// Move は ContinuousMove を送る。x=パン速度, y=チルト速度 (-1.0〜1.0)。
// 動き続けるので、止めるには Stop を呼ぶこと。
func (c *Controller) Move(x, y float64) error {
	x, y = clamp(x), clamp(y)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.initLocked(); err != nil {
		return err
	}

	body := fmt.Sprintf(
		`<tptz:ContinuousMove xmlns:tptz="http://www.onvif.org/ver20/ptz/wsdl">`+
			`<tptz:ProfileToken>%s</tptz:ProfileToken>`+
			`<tptz:Velocity><tt:PanTilt x="%.2f" y="%.2f"/></tptz:Velocity>`+
			`</tptz:ContinuousMove>`,
		c.token, x, y)

	if _, err := c.client.Request(c.ptzURL, body); err != nil {
		c.reset()
		return fmt.Errorf("ptz: move: %w", err)
	}
	return nil
}

// Stop は PTZ の動作を止める。
func (c *Controller) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.initLocked(); err != nil {
		return err
	}

	body := fmt.Sprintf(
		`<tptz:Stop xmlns:tptz="http://www.onvif.org/ver20/ptz/wsdl">`+
			`<tptz:ProfileToken>%s</tptz:ProfileToken>`+
			`<tptz:PanTilt>true</tptz:PanTilt>`+
			`</tptz:Stop>`,
		c.token)

	if _, err := c.client.Request(c.ptzURL, body); err != nil {
		c.reset()
		return fmt.Errorf("ptz: stop: %w", err)
	}
	return nil
}

// reset は接続を破棄して次回呼び出しで再初期化させる (呼び出し側で mu を保持)。
func (c *Controller) reset() {
	c.client = nil
	c.ptzURL = ""
	c.token = ""
}

// ServeHTTP は POST {"x": -1..1, "y": -1..1} を受ける。
// x=y=0 は Stop として扱う (押しっぱなし → 離す UI を想定)。
func (c *Controller) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var err error
	if req.X == 0 && req.Y == 0 {
		err = c.Stop()
	} else {
		err = c.Move(req.X, req.Y)
	}
	if err != nil {
		log.Printf("%v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func clamp(v float64) float64 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}
