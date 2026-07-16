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
 ├─ UIブリッジ: 点呼UI 向けタブレット時代ブリッジ互換 WS (internal/uibridge 共通部)
 │   ├─ NFC:     ws://127.0.0.1:9876 (internal/nfcbridge, rust-nfc-bridge 互換)
 │   ├─ 体温血圧: ws://127.0.0.1:9877 (internal/blebridge, ble-medical-gateway 互換)
 │   └─ FC-1200: ws://127.0.0.1:9878 (internal/fc1200bridge)
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

## CoreS3 連携 (WS ハブ + UI ブリッジ)

CoreS3 (ippoan/alc-app-s3) は `ws://<GW-IP>:9000` に WS クライアントとして接続する。
メッセージは JSON テキストフレーム 1 通 = 1 オブジェクト:

```jsonc
// CoreS3 → GW
{"src":"cores3","type":"hello","device":"cores3-01","fw":"v0.4.0"}
{"src":"cores3","type":"nfc_read","card_id":"04A1B2C3D4","card_type":"mifare"}
// payload は cf-alc-recorder へ送る measurement と同じ ble-medical-gateway 互換 JSON
{"src":"cores3","type":"measurement","kind":"temperature","payload":{"type":"temperature","value":36.5,"unit":"celsius","measured_at":20260716120000}}
{"src":"cores3","type":"measurement","kind":"blood_pressure","payload":{"type":"blood_pressure","systolic":120,"diastolic":80,"pulse":60,"unit":"mmHg"}}
{"src":"cores3","type":"measurement","kind":"alcohol","payload":{"type":"alcohol","value":0.15,"unit":"mg/L","result":"normal","use_count":42}}
{"src":"cores3","type":"ble_status","thermo":true,"bp":false}          // 体温計・血圧計の BLE 接続状態
{"src":"cores3","type":"fc1200_event","payload":{"type":"state_changed","to":"waiting_breath"}}  // Fc1200Event 互換

// GW → CoreS3 (command はタブレット時代の Android ブリッジ互換:
// reset / sensor_lifetime / memory_read / memory_complete / date_update:<dt> / connect)
{"src":"gw","type":"ble_command","command":"reset"}
{"src":"gw","type":"fc1200_command","command":"reset"}
```

- 未知の `type` / `kind` は無視 (ログのみ)
- keep-alive はサーバー側から WS ping (30s 間隔、90s 無応答で切断)
- `hello` 未受信でも接続時点で `cores3@<addr>` としてデバイス登録される

**自動発見 (internal/discovery)**: GW は 5 秒ごとに UDP 9001 へ自分のハブ URL を
ブロードキャストする:

```jsonc
{"src":"alc-gw","type":"beacon","ws":"ws://192.168.11.5:9000","fw":"v0.1.5"}
```

CoreS3 は UDP 9001 を聴き、`GW URL` (NVS) 未設定ならこの URL に自動接続する —
同一 LAN に置くだけで配線される。`GW URL` を設定した場合はそちらが優先
(セグメント跨ぎ・GW 複数台などの明示指定用)。

GW は同時に、点呼UI (alc-app) がタブレット時代から話せるブリッジ互換 WS を
loopback に 3 本立てて中継する:

| ポート | 互換元 | alc-app 側 | 中継内容 |
|---|---|---|---|
| 127.0.0.1:9876 | rust-nfc-bridge | `useNfcWebSocket` | CoreS3 接続中は `status.readers` にデバイス名 → 「NFC リーダー未検出」が「NFC 待機中」になる。`nfc_read` は `employee_id` に変換 |
| 127.0.0.1:9877 | ble-medical-gateway | `useBleGateway` | `ready`/`heartbeat`(thermo/bp) + 体温・血圧 payload を無変換で配信。UI の `{"command":"reset"}` は CoreS3 へ |
| 127.0.0.1:9878 | Android FC-1200 ブリッジ | `useFc1200Serial` | `alcohol` を `measurement_result` に変換、`fc1200_event` は無変換。UI のコマンドは CoreS3 へ |

本物のブリッジ (rust-nfc-bridge 等) が同じポートを掴んでいる場合は Listen に
失敗してそちらへ譲る (USB 機器直結のフォールバック)。

**WebSerial の無効化**: WebView2 は WebSerial 対応のため、そのままだと alc-app の
`useBleGateway`/`useFc1200Serial` が WS ブリッジにフォールバックしない。GW には
シリアル機器を直結しない構成なので、`WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS=--disable-blink-features=Serial`
を main() で設定して WebView 内の `navigator.serial` を消している
(環境変数が既に設定されていればそちらを優先)。

デバッグ (`127.0.0.1:11984`):

```powershell
irm http://127.0.0.1:11984/api/hub/status                    # 接続中デバイス一覧
irm http://127.0.0.1:11984/api/nfc/read -Method Post -Body '{"card_id":"TEST01"}' -ContentType application/json
                                                              # 読取のテスト注入
# CoreS3 からのメッセージ受信を装う (measurement / ble_status / fc1200_event)
irm http://127.0.0.1:11984/api/hub/inject -Method Post -ContentType application/json -Body `
  '{"src":"cores3","type":"measurement","kind":"temperature","payload":{"type":"temperature","value":36.5,"unit":"celsius"}}'
```

## 設定

- 通常はトレイメニューの「設定」から (→ `%LOCALAPPDATA%\alc-gw\config.json`)
- 環境変数 `ALC_GW_RTSP_URL` を設定すると config.json より優先 (開発用)

## リリース (Release Wave)

1. Actions タブ → **Tag Release** → Run workflow (bump は通常 patch) で
   semver タグが自動採番・push される (`gh workflow run tag-release.yml` でも可)
2. `v*` タグの push で release.yml が連鎖発火し、`wails build -nsis` で
   `alc-gw-amd64-installer.exe` を作り **latest にしない Release (= 配信保留)** として
   添付、[ci-dashboard の /release-wave](https://ci-dashboard.ippoan.org/release-wave)
   の Pending releases に報告する。staging 環境があるわけではない —
   自動更新が releases/latest しか見ないことを利用した、単なる配信ゲート
3. 必要なら Pending releases のリンク (= ただの Release ページ) からインストーラを
   手動インストールして検証し、**Flip** で latest に昇格 → インストール済み GW の
   自動更新 (起動 1 分後 or トレイの「更新を確認」) が新版を取り込む。
   Flip するまでフリートは一切更新されない = 営業時間を避ける等、配信タイミングを選べる
4. ロールバックは `gh release edit <旧タグ> --latest`

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

- [x] WS ハブ (ws://:9000) + WS メッセージ仕様 (hello / nfc_read / measurement / ble_status / fc1200_event / *_command)
- [ ] CoreS3 firmware 側の GW WS クライアント (ippoan/alc-app-s3) — NFC 読取は alc-app#100 参照
- [ ] face-api 生体認証の移植 (`@vladmandic/human`、入力 = Tapo C212)
- [ ] WebAuthn 承認 (auth-worker 連携、RP ID = ippoan.org)
- [ ] 対面点呼 WAN 断時のローカル承認 (UserConsentVerifier + ログ同期)
- [ ] Windows キオスク化 / 常駐化 / OTA / watchdog
- [ ] TenkoCall への映像送出
