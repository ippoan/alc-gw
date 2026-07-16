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
 ├─ WSハブ: CoreS3 受け ws://<GW-IP>:9000 (TODO)
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

- [ ] WS ハブ (ws://:9000) + WS メッセージ仕様 (JSON スキーマ、src/type/target)
- [ ] face-api 生体認証の移植 (`@vladmandic/human`、入力 = Tapo C212)
- [ ] WebAuthn 承認 (auth-worker 連携、RP ID = ippoan.org)
- [ ] 対面点呼 WAN 断時のローカル承認 (UserConsentVerifier + ログ同期)
- [ ] Windows キオスク化 / 常駐化 / OTA / watchdog
- [ ] TenkoCall への映像送出
