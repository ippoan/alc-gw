package main

import (
	"embed"
	"net/http"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"alc-gw/internal/stream"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := NewApp()

	// カメラ映像 (Tapo C212) の WHEP エンドポイント。
	// AssetServer の Handler に載せることで WebView と同一オリジンになる。
	streamServer := stream.NewServer(os.Getenv("ALC_GW_RTSP_URL"))
	mux := http.NewServeMux()
	mux.Handle("/api/whep", streamServer)

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
