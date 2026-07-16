# alc-gw

Windows GW 常駐アプリ — Wails (Go) + go2rtc ライブラリ組込み。
点呼システムの GW 側 (タッチ画面) を担う単一 exe。

設計の出自: [ippoan/alc-app#120](https://github.com/ippoan/alc-app/issues/120)

## 構成

```
[Windows GW + タッチ画面] ← 本リポジトリ (Wails 単一 exe)
 ├─ WebView: 点呼UI (Vue 3 + TypeScript)
 ├─ go2rtc (pkg import): C212 RTSP → WebRTC 変換 (WHEP, /api/whep)
 ├─ face-api: 生体認証 (TODO: alc-app タブレット実装から移植)
 ├─ WSハブ: CoreS3 受け ws://<GW-IP>:9000 (internal/hub)
 ├─ NFCブリッジ: rust-nfc-bridge 互換 ws://127.0.0.1:9876 (internal/nfcbridge)
 └─ 常駐化: Windows サービス or タスクスケジューラ (TODO)

[CoreS3] 入出力端末 (WS クライアント) → ippoan/alc-app-s3
```

## 実装済み

- Wails v2 スケルトン (vue-ts)
- go2rtc を **Go ライブラリとして import** (sidecar なし・プロセス 1 個)
  - `internal/stream`: RTSP producer → WebRTC consumer の配線
  - go2rtc の `internal/` は import 不可のため `pkg/` プリミティブのみ使用。
    pkg API は semver 保証がないので、バージョン追従の影響は `internal/stream` に閉じ込める
- WHEP エンドポイント `/api/whep` (Wails AssetServer の Handler に同居 = 同一オリジン、CORS 不要)
- LAN 内完結: ICE 候補は loopback + RFC1918 UDP4 のみ・固定ポート :8555 (STUN/TURN なし)
- パンチルト (`internal/ptz`): ONVIF PTZ ContinuousMove/Stop。`POST /api/ptz {"x":-1..1,"y":-1..1}`
- システムトレイ常駐 (energye/systray): 右クリックメニューに 表示/設定/更新を確認/終了。
  ウィンドウの閉じるボタンは最小化 (終了はトレイから)
- 設定画面 (トレイ → 設定): RTSP URL を `%LOCALAPPDATA%\alc-gw\config.json` に保存、即時反映
- 自動更新 (`internal/update`): GitHub Releases の latest を起動 1 分後に確認、
  新しければ NSIS インストーラをサイレント適用して再起動。トレイから手動確認も可
- WS ハブ (`internal/hub`) + NFC ブリッジ (`internal/nfcbridge`): 下記「CoreS3 連携」参照

## CoreS3 連携 (WS ハブ + NFC ブリッジ)

CoreS3 (ippoan/alc-app-s3) は `ws://<GW-IP>:9000` に WS クライアントとして接続する。
メッセージは JSON テキストフレーム 1 通 = 1 オブジェクト:

```jsonc
// CoreS3 → GW
{"src":"cores3","type":"hello","device":"cores3-01","fw":"v0.4.0"}
{"src":"cores3","type":"nfc_read","card_id":"04A1B2C3D4","card_type":"mifare"}
```

- 未知の `type` は無視 (ログのみ)。測定値などは今後この仕様に追記する
- keep-alive はサーバー側から WS ping (30s 間隔、90s 無応答で切断)
- `hello` 未受信でも接続時点で `cores3@<addr>` としてデバイス登録される

GW は同時に **rust-nfc-bridge 互換** の WS サーバーを `ws://127.0.0.1:9876` に立てる。
WebView 内の点呼UI (alc-app の `useNfcWebSocket`) はここに接続し、

- CoreS3 が接続中 → `{"type":"status","readers":["cores3-01"],...}` が配られ、
  NfcStatus の表示が「NFC リーダー未検出」→「NFC 待機中」に変わる
- CoreS3 の `nfc_read` → `{"type":"nfc_read","employee_id":"<card_id>"}` に変換して配信

本物の rust-nfc-bridge が 9876 を掴んでいる場合は Listen に失敗してそちらへ譲る
(USB NFC リーダー併用のフォールバック)。

デバッグ (`127.0.0.1:11984`):

```powershell
irm http://127.0.0.1:11984/api/hub/status                    # 接続中デバイス一覧
irm http://127.0.0.1:11984/api/nfc/read -Method Post -Body '{"card_id":"TEST01"}' -ContentType application/json
                                                              # 読取のテスト注入
```

## 設定

- 通常はトレイメニューの「設定」から (→ `%LOCALAPPDATA%\alc-gw\config.json`)
- 環境変数 `ALC_GW_RTSP_URL` を設定すると config.json より優先 (開発用)

## リリース

`v*` タグを push すると GitHub Actions (windows-latest) が
`wails build -nsis` で `alc-gw-amd64-installer.exe` を作り Release に添付する。
バージョンは `-ldflags -X main.version=<tag>` で埋め込まれ、自動更新の比較に使われる。

## 開発

```bash
wails dev                 # 開発モード (ホットリロード)
wails build               # 単一 exe → build/bin/alc-gw.exe (~15MB)
go build ./... && go vet ./...
cd frontend && npx vue-tsc --noEmit
```

要件: Go 1.25+, Node.js, Wails CLI v2 (`go install github.com/wailsapp/wails/v2/cmd/wails@latest`)

## ロードマップ (alc-app#120 の TODO より)

- [x] WS ハブ (ws://:9000) + WS メッセージ仕様 (hello / nfc_read — 測定系は今後追加)
- [ ] CoreS3 firmware 側の WS クライアント + NFC 読取 (ippoan/alc-app-s3、alc-app#100)
- [ ] face-api 生体認証の移植 (`@vladmandic/human`、入力 = Tapo C212)
- [ ] WebAuthn 承認 (auth-worker 連携、RP ID = ippoan.org)
- [ ] 対面点呼 WAN 断時のローカル承認 (UserConsentVerifier + ログ同期)
- [ ] Windows キオスク化 / 常駐化 / OTA / watchdog
- [ ] TenkoCall への映像送出
