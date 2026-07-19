package main

import (
	"context"
	"log"

	"alc-gw/internal/config"
	"alc-gw/internal/ptz"
	"alc-gw/internal/stream"
	"alc-gw/internal/update"
	"alc-gw/internal/whip"
)

// App struct
type App struct {
	ctx context.Context

	stream *stream.Server
	ptz    *ptz.Controller
	whip   *whip.Session
}

// NewApp creates a new App application struct
func NewApp(streamServer *stream.Server, ptzCtl *ptz.Controller, whipSession *whip.Session) *App {
	return &App{stream: streamServer, ptz: ptzCtl, whip: whipSession}
}

// startup is called when the app starts. The context is saved
// so we can call the runtime methods
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

// Settings は設定画面に渡す現在値。
type Settings struct {
	RTSPURL string `json:"rtspUrl"`
	Version string `json:"version"`
}

func (a *App) GetSettings() Settings {
	return Settings{
		RTSPURL: config.Load().RTSPURL,
		Version: version,
	}
}

// SaveSettings は設定を保存し、即座にカメラ接続へ反映する。
// WHIP 関連フィールドは config.json 手書き専用 (SettingsDialog は RTSPURL の
// み扱う) なので、既存設定を読み直してから RTSPURL だけ上書きする
// — でないと WHIP 設定がここで消える。
func (a *App) SaveSettings(rtspURL string) error {
	cfg := config.Load()
	cfg.RTSPURL = rtspURL
	if err := config.Save(cfg); err != nil {
		return err
	}
	a.stream.SetSource(rtspURL)
	a.ptz.SetSource(rtspURL)
	a.whip.Start(whip.Config{RTSPURL: cfg.WHIPRTSPURL, WHIPURL: cfg.WHIPURL, Token: cfg.WHIPToken})
	log.Printf("config: rtsp source updated")
	return nil
}

// CheckUpdate は手動の更新確認。更新があれば適用してアプリを終了する。
// 戻り値: 適用した新バージョンのタグ (更新なしは空文字)。
func (a *App) CheckUpdate() (string, error) {
	rel, err := update.Check(version)
	if err != nil || rel == nil {
		return "", err
	}
	if err = update.Apply(rel); err != nil {
		return "", err
	}
	go func() {
		quitApp(a.ctx)
	}()
	return rel.Tag, nil
}
