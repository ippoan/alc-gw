package main

import (
	"context"
	"log"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"alc-gw/internal/config"
	"alc-gw/internal/ptz"
	"alc-gw/internal/stream"
	"alc-gw/internal/update"
)

// App struct
type App struct {
	ctx context.Context

	stream *stream.Server
	ptz    *ptz.Controller
}

// NewApp creates a new App application struct
func NewApp(streamServer *stream.Server, ptzCtl *ptz.Controller) *App {
	return &App{stream: streamServer, ptz: ptzCtl}
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
func (a *App) SaveSettings(rtspURL string) error {
	if err := config.Save(config.Config{RTSPURL: rtspURL}); err != nil {
		return err
	}
	a.stream.SetSource(rtspURL)
	a.ptz.SetSource(rtspURL)
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
		runtime.Quit(a.ctx)
	}()
	return rel.Tag, nil
}
