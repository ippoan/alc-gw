package main

import (
	"context"
	"os"
)

// App struct
type App struct {
	ctx context.Context
}

// NewApp creates a new App application struct
func NewApp() *App {
	return &App{}
}

// startup is called when the app starts. The context is saved
// so we can call the runtime methods
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

// StreamConfigured はカメラ (RTSP ソース) が設定済みかを返す
func (a *App) StreamConfigured() bool {
	return os.Getenv("ALC_GW_RTSP_URL") != ""
}
