package main

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/energye/systray"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"alc-gw/internal/blebridge"
	"alc-gw/internal/config"
	"alc-gw/internal/discovery"
	"alc-gw/internal/fc1200bridge"
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

// quitting は「本当に終了する」意思表示。runtime.Quit は内部で OnBeforeClose を
// 呼び、true (= キャンセル) が返ると何もしない (wails v2 Frontend.Quit)。
// OnBeforeClose が常に true (閉じる = 最小化) の本アプリでは、このフラグ無しだと
// トレイの「終了」も自動更新後の再起動も握り潰される。
var quitting atomic.Bool

// quitApp はトレイ掃除 → 本終了。OnBeforeClose の最小化ガードを quitting で抜ける
func quitApp(ctx context.Context) {
	quitting.Store(true)
	systray.Quit()
	runtime.Quit(ctx)
}

// debugAddr はデバッグ・外部連携用の HTTP 待ち受け (Wails の AssetServer は
// WebView 内からしか届かないため、疎通確認用に localhost でも同じ mux を公開する)
const debugAddr = "127.0.0.1:11984"

// hubAddr は CoreS3 (alc-app-s3) を受ける WS ハブ (LAN 内、alc-app#120)
const hubAddr = ":9000"

// hubPort / beaconPort は CoreS3 の GW 自動発見 (internal/discovery) 用。
// GW が UDP beaconPort へ自分の WS ハブ URL をブロードキャストし、CoreS3 は
// `GW URL` 未設定ならそれに自動接続する
const (
	hubPort    = 9000
	beaconPort = 9001
)

// 点呼UI (alc-app) が接続するブリッジ WS 群。ポートは Android タブレット時代の
// ブリッジ互換 (useNfcWebSocket / useBleGateway / useFc1200Serial の固定値)。
// 同名の本物のブリッジが動いていればポートが取れず、そちらに譲る
const (
	nfcBridgeAddr    = "127.0.0.1:9876"
	bleBridgeAddr    = "127.0.0.1:9877"
	fc1200BridgeAddr = "127.0.0.1:9878"
)

func main() {
	setupLogFile()

	// WebView2 (Chromium) は WebSerial 対応のため、点呼UI の useBleGateway /
	// useFc1200Serial が WS ブリッジ (9877/9878) にフォールバックしない。
	// GW にシリアル機器は直結しない構成 (FC-1200 も BLE GW も CoreS3 側) なので
	// WebView では WebSerial を無効化して WS 経路に落とす。
	// (Wails v2 に browser args のオプションが無いため WebView2 loader の環境変数で渡す)
	if os.Getenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS") == "" {
		_ = os.Setenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS", "--disable-blink-features=Serial")
	}

	cfg := config.Load()

	// カメラ映像 (Tapo C212) の WHEP エンドポイント。
	// AssetServer の Handler に載せることで WebView と同一オリジンになる。
	streamServer := stream.NewServer(cfg.RTSPURL)
	ptzCtl := ptz.FromRTSP(cfg.RTSPURL)

	// CoreS3 の NFC 読取・体温・血圧・FC-1200 を、点呼UI が話せる
	// タブレット時代のブリッジ互換 WS (9876/9877/9878) へ中継する。
	// GW → CoreS3 方向のコマンド (BLE 再スキャン、FC-1200 操作) は hub 経由。
	nfcBridge := nfcbridge.New(version)
	var hubServer *hub.Hub
	bleBridge := blebridge.New(version, func(cmd string) { hubServer.SendCommand("ble_command", cmd) })
	fcBridge := fc1200bridge.New(func(cmd string) { hubServer.SendCommand("fc1200_command", cmd) })
	hubServer = hub.New(hub.Callbacks{
		Devices: func(devices []string) {
			nfcBridge.SetReaders(devices)
			if len(devices) == 0 {
				// CoreS3 が全て切断 → BLE 機器の接続状態もリセット
				bleBridge.SetDeviceState(false, false)
			}
		},
		NfcRead: nfcBridge.InjectRead,
		Measurement: func(kind string, payload []byte) {
			switch kind {
			case "temperature", "blood_pressure":
				bleBridge.Measurement(payload)
			case "alcohol":
				fcBridge.Alcohol(payload)
			default:
				log.Printf("hub: 未知の measurement kind: %q", kind)
			}
		},
		BleStatus:   bleBridge.SetDeviceState,
		Fc1200Event: fcBridge.Event,
	})
	listen := func(name string, srv interface{ ListenAndServe(string) error }, addr string) {
		go func() {
			if err := srv.ListenAndServe(addr); err != nil {
				log.Printf("%s: listen: %v (本物のブリッジ稼働中ならそちらを使う)", name, err)
			}
		}()
	}
	listen("nfc-bridge", nfcBridge, nfcBridgeAddr)
	listen("ble-bridge", bleBridge, bleBridgeAddr)
	listen("fc1200-bridge", fcBridge, fc1200BridgeAddr)
	listen("hub", hubServer, hubAddr)
	// CoreS3 の GW 自動発見: WS ハブ URL を UDP ブロードキャストし続ける
	discovery.Start(beaconPort, hubPort, version)

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
	// テスト用: CoreS3 からのメッセージ受信を装う (measurement / ble_status 等)
	mux.HandleFunc("/api/hub/inject", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST のみ", http.StatusMethodNotAllowed)
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
		if err != nil || len(data) == 0 {
			http.Error(w, "CoreS3 メッセージの JSON が必要です", http.StatusBadRequest)
			return
		}
		hubServer.Inject(data)
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
		// 二重起動防止: 更新の入れ違い等で 2 個目が起動されても、既存
		// インスタンスのウィンドウを前面化して自分は即終了する (旧プロセスの
		// ゾンビトレイアイコンが残る事故の再発防止)
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "alc-gw-1c1f7a58-9b1e-4a53-8a44-4b2f7f6f2a10",
			OnSecondInstanceLaunch: func(_ options.SecondInstanceData) {
				if app.ctx != nil {
					runtime.WindowUnminimise(app.ctx)
					runtime.WindowShow(app.ctx)
				}
			},
		},
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
		// 閉じるボタン = 最小化 (常時表示・終了はトレイの「終了」から)。
		// quitting 時は素通し — runtime.Quit もここを通るため、ガードしないと
		// トレイの終了・更新後の再起動が最小化に化ける
		OnBeforeClose: func(ctx context.Context) bool {
			if quitting.Load() {
				return false
			}
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
			quitApp(ctx)
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
	quitApp(ctx)
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
	quitApp(ctx)
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
