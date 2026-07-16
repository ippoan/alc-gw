package main

import (
	"embed"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"alc-gw/internal/ptz"
	"alc-gw/internal/stream"
)

//go:embed all:frontend/dist
var assets embed.FS

// debugAddr はデバッグ・外部連携用の HTTP 待ち受け (Wails の AssetServer は
// WebView 内からしか届かないため、疎通確認用に localhost でも同じ mux を公開する)
const debugAddr = "127.0.0.1:11984"

func main() {
	setupLogFile()

	app := NewApp()

	// カメラ映像 (Tapo C212) の WHEP エンドポイント。
	// AssetServer の Handler に載せることで WebView と同一オリジンになる。
	streamServer := stream.NewServer(os.Getenv("ALC_GW_RTSP_URL"))
	mux := http.NewServeMux()
	mux.Handle("/api/whep", streamServer)
	mux.HandleFunc("/api/stream/status", streamServer.Status)
	mux.HandleFunc("/debug", stream.DebugPage)

	// パンチルト (ONVIF PTZ)。RTSP と同じカメラアカウントを使う
	mux.Handle("/api/ptz", ptz.FromRTSP(os.Getenv("ALC_GW_RTSP_URL")))

	go func() {
		if err := http.ListenAndServe(debugAddr, mux); err != nil {
			log.Printf("debug listener: %v", err)
		}
	}()

	err := wails.Run(&options.App{
		Title:  "alc-gw",
		Width:  1024,
		Height: 768,
		AssetServer: &assetserver.Options{
			Assets:  assets,
			Handler: mux,
		},
		BackgroundColour: &options.RGBA{R: 27, G: 38, B: 54, A: 1},
		OnStartup:        app.startup,
		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}

// setupLogFile は log 出力を %LOCALAPPDATA%\alc-gw\gw.log へ送る
// (GUI exe は stdout が見えないため)。失敗しても起動は続行する。
func setupLogFile() {
	dir := os.Getenv("LOCALAPPDATA")
	if dir == "" {
		return
	}
	dir = filepath.Join(dir, "alc-gw")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "gw.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	log.SetOutput(f)
}
