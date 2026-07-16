package main

import (
	"context"
	"embed"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/energye/systray"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"alc-gw/internal/config"
	"alc-gw/internal/hub"
	"alc-gw/internal/nfcbridge"
	"alc-gw/internal/ptz"
	"alc-gw/internal/stream"
	"alc-gw/internal/update"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed build/windows/icon.ico
var trayIcon []byte

// version はリリースビルド時に -ldflags "-X main.version=v0.1.0" で注入される
var version = "dev"

// debugAddr はデバッグ・外部連携用の HTTP 待ち受け (Wails の AssetServer は
// WebView 内からしか届かないため、疎通確認用に localhost でも同じ mux を公開する)
const debugAddr = "127.0.0.1:11984"

// hubAddr は CoreS3 (alc-app-s3) を受ける WS ハブ (LAN 内、alc-app#120)
const hubAddr = ":9000"

// nfcBridgeAddr は rust-nfc-bridge 互換 WS。WebView 内の点呼UI が接続する。
// 本物の rust-nfc-bridge が動いていればポートが取れず、そちらに譲る
const nfcBridgeAddr = "127.0.0.1:9876"

func main() {
	setupLogFile()

	cfg := config.Load()

	// カメラ映像 (Tapo C212) の WHEP エンドポイント。
	// AssetServer の Handler に載せることで WebView と同一オリジンになる。
	streamServer := stream.NewServer(cfg.RTSPURL)
	ptzCtl := ptz.FromRTSP(cfg.RTSPURL)

	// CoreS3 の NFC 読取 → nfc-bridge 互換 WS (→ 点呼UI の NfcStatus) へ中継
	nfcBridge := nfcbridge.New(version)
	hubServer := hub.New(nfcBridge.SetReaders, nfcBridge.InjectRead)
	go func() {
		if err := nfcBridge.ListenAndServe(nfcBridgeAddr); err != nil {
			log.Printf("nfc-bridge: listen: %v (rust-nfc-bridge 稼働中ならそちらを使う)", err)
		}
	}()
	go func() {
		if err := hubServer.ListenAndServe(hubAddr); err != nil {
			log.Printf("hub: listen: %v", err)
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/api/whep", streamServer)
	mux.HandleFunc("/api/stream/status", streamServer.Status)
	mux.HandleFunc("/debug", stream.DebugPage)
	mux.Handle("/api/ptz", ptzCtl)
	mux.HandleFunc("/api/hub/status", hubServer.Status)
	// テスト用の読取注入 (CoreS3 firmware 未実装でも E2E 確認できる)
	mux.HandleFunc("/api/nfc/read", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST のみ", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			CardID string `json:"card_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.CardID == "" {
			http.Error(w, `{"card_id":"..."} が必要です`, http.StatusBadRequest)
			return
		}
		nfcBridge.InjectRead(body.CardID)
		w.WriteHeader(http.StatusNoContent)
	})

	go func() {
		// 点呼UI (https://alc.ippoan.org 等) からローカル API を呼べるよう
		// CORS + Private Network Access を許可 (loopback bind なので
		// 同一マシンのブラウザ/WebView からしか届かない)
		if err := http.ListenAndServe(debugAddr, cors(mux)); err != nil {
			log.Printf("debug listener: %v", err)
		}
	}()

	app := NewApp(streamServer, ptzCtl)

	err := wails.Run(&options.App{
		Title:  "alc-gw",
		Width:  1024,
		Height: 768,
		AssetServer: &assetserver.Options{
			Assets:  assets,
			Handler: mux,
		},
		BackgroundColour: &options.RGBA{R: 27, G: 38, B: 54, A: 1},
		OnStartup: func(ctx context.Context) {
			app.startup(ctx)
			go systray.Run(trayReady(ctx, app), nil)
			go autoUpdate(ctx)
		},
		// 閉じるボタン = 最小化 (常時表示・終了はトレイの「終了」から)
		OnBeforeClose: func(ctx context.Context) bool {
			runtime.WindowMinimise(ctx)
			return true
		},
		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}

// trayReady はシステムトレイのメニューを組み立てる。
// 右クリックでメニュー表示、左クリック/「表示」でウィンドウ復帰。
func trayReady(ctx context.Context, app *App) func() {
	return func() {
		systray.SetIcon(trayIcon)
		systray.SetTooltip("alc-gw " + version)

		show := func() {
			runtime.WindowUnminimise(ctx)
			runtime.WindowShow(ctx)
		}

		systray.SetOnClick(func(menu systray.IMenu) { show() })
		systray.SetOnRClick(func(menu systray.IMenu) { _ = menu.ShowMenu() })

		mShow := systray.AddMenuItem("表示", "ウィンドウを前面に出す")
		mShow.Click(show)

		// WebView は点呼UI (外部サイト) を表示しているため、設定は
		// config.json をメモ帳で直接編集してもらう。閉じたら即反映
		mSettings := systray.AddMenuItem("設定", "config.json を編集")
		mSettings.Click(func() {
			go func() {
				cfg := config.Load()
				_ = config.Save(cfg) // 未作成ならひな形を書き出す
				cmd := exec.Command("notepad", config.Path())
				if err := cmd.Run(); err != nil {
					log.Printf("config: editor: %v", err)
					return
				}
				cfg = config.Load()
				app.stream.SetSource(cfg.RTSPURL)
				app.ptz.SetSource(cfg.RTSPURL)
				log.Printf("config: reloaded after edit")
			}()
		})

		mUpdate := systray.AddMenuItem("更新を確認", "新しいバージョンを確認")
		mUpdate.Click(func() {
			go checkUpdateInteractive(ctx)
		})

		systray.AddSeparator()

		mQuit := systray.AddMenuItem("終了", "アプリを終了する")
		mQuit.Click(func() {
			systray.Quit()
			runtime.Quit(ctx)
		})
	}
}

// autoUpdate は起動 1 分後に更新を確認し、あれば黙って適用して再起動する。
func autoUpdate(ctx context.Context) {
	time.Sleep(time.Minute)
	rel, err := update.Check(version)
	if err != nil {
		log.Printf("update: check failed: %v", err)
		return
	}
	if rel == nil {
		return
	}
	log.Printf("update: applying %s (current %s)", rel.Tag, version)
	if err = update.Apply(rel); err != nil {
		log.Printf("update: apply failed: %v", err)
		return
	}
	runtime.Quit(ctx)
}

// checkUpdateInteractive はトレイメニューからの手動更新確認。
// 結果をダイアログで知らせる。
func checkUpdateInteractive(ctx context.Context) {
	rel, err := update.Check(version)
	if err != nil {
		_, _ = runtime.MessageDialog(ctx, runtime.MessageDialogOptions{
			Type: runtime.ErrorDialog, Title: "更新確認", Message: "確認に失敗しました: " + err.Error(),
		})
		return
	}
	if rel == nil {
		_, _ = runtime.MessageDialog(ctx, runtime.MessageDialogOptions{
			Type: runtime.InfoDialog, Title: "更新確認", Message: "最新版です (" + version + ")",
		})
		return
	}
	choice, _ := runtime.MessageDialog(ctx, runtime.MessageDialogOptions{
		Type: runtime.QuestionDialog, Title: "更新確認",
		Message: rel.Tag + " が利用可能です。更新して再起動しますか?",
	})
	if choice != "Yes" {
		return
	}
	if err = update.Apply(rel); err != nil {
		_, _ = runtime.MessageDialog(ctx, runtime.MessageDialogOptions{
			Type: runtime.ErrorDialog, Title: "更新確認", Message: "更新に失敗しました: " + err.Error(),
		})
		return
	}
	runtime.Quit(ctx)
}

// cors は同一マシン上のブラウザ/WebView (https://alc.ippoan.org 等) から
// ローカル API への呼び出しを許可する。
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		// Chrome の Private Network Access preflight 対応
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// setupLogFile は log 出力を %LOCALAPPDATA%\alc-gw\gw.log へ送る
// (GUI exe は stdout が見えないため)。失敗しても起動は続行する。
func setupLogFile() {
	dir := config.Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "gw.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	log.SetOutput(f)
}
