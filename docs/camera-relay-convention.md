# カメラ中継規約 (C212 全景配信)

遠隔点呼で C212 (Tapo) の全景映像を拠点外の管理者へ届けるための規約。
全拠点「device (本ファーム/アプリ) が signaling room へ常時接続する」
方式に統一する方針 ([alc-gw#6](https://github.com/ippoan/alc-gw/issues/6))
のもと、publisher 実装は拠点構成で 2 種類に分かれるが、シグナリング
サーバーから見た規約は完全に共通:

```
標準拠点:    C212 ─RTSP─▶ Unit PoE-P4 (ippoan/alc-gw-p4, esp_peer) ─┐
Windows拠点: C212 ─RTSP─▶ alc-gw (本リポジトリ, internal/whip)     ─┤
                                                                    ▼
                                          signaling room (DO, role=device)
                                                                    │
                                     SDP/ICE 交換 (peer_joinedで開始)
                                                                    ▼
                                              管理者ブラウザ (role=admin)
                                                    │
                                    直接 P2P WebRTC (STUN のみ、DOは経由しない)
```

**方針転換の経緯 (2026-07-19)**: 当初は WHIP (RFC 9725, HTTP POST/DELETE)
+ クラウド SFU 方式だったが、以下 2 点により前提が変わり、この規約に
置き換えた ([ippoan/alc-app#129](https://github.com/ippoan/alc-app/issues/129)
に検討経緯):

1. 同時複数視聴 (1 拠点を複数管理者が同時に見る) は不要と判明。
   SFU が解決する「1 publisher → N viewer のファンアウト」がそもそも
   要らない
2. [ippoan/alc-app](https://github.com/ippoan/alc-app) の
   `cf-alc-signaling` (Durable Objects + Hibernatable WebSockets による
   1:1 シグナリング、TenkoCall の遠隔点呼で稼働中) という資産が既に
   あり、新規の管理型 SFU (Cloudflare Realtime 等) を導入する理由が
   無くなった

`internal/whip` (パッケージ名は歴史的経緯でそのまま) の Go/pion 実装が
P4 ファーム実装のリファレンス。この文書の規約に従えばどちらの
device 実装も同じ signaling room 設定 (エンドポイント・トークン) で
受けられる。

## エンドポイント

```
wss://<signaling>/cam-room/<拠点ID>?role=device|admin
```

- 1 拠点 = 1 ルーム = 1 URL。ルーム ID は **拠点 ID そのもの**
  (点呼システム側の拠点 ID と同一の値を使う。新規の採番は行わない)
- device 役・admin 役それぞれ 1 本まで (2 本目の同role接続は 409 で拒否)
- 実装: `ippoan/alc-app` の `cf-alc-signaling`、`CameraSignalingRoom`
  (Durable Object)。TenkoCall の着信通知に使う `SignalingRoom` /
  `RoomRegistry` とは完全に独立 — device の接続が誤って着信 UI を
  鳴らすことはない

## 認証

- `Authorization: Bearer <拠点トークン>` を WebSocket 接続時のヘッダに
  付与する
- **v1 時点で DO 側はこのヘッダを検証しない**。将来
  [ippoan/auth-worker#406](https://github.com/ippoan/auth-worker/issues/406)
  (role=gateway device credential からの mint) 完了後に検証を追加する
  想定。それまでは接続元 URL/ルーム ID の非公開性のみに依存する
- トークンは拠点ごとに 1 本。v1 は配布・失効の自動化はせず、キッティング時に
  設定へ書き込む (下記「設定」参照)

## メッセージプロトコル

WebSocket 確立後、JSON テキストフレームでやり取りする
(`ippoan/alc-app` の `cf-alc-signaling/src/camera-signaling-room.ts` が
一次定義):

```jsonc
// device → server → admin
{ "type": "sdp_offer", "sdp": "v=0..." }

// admin → server → device
{ "type": "sdp_answer", "sdp": "v=0..." }

// 双方向 (任意、下記「ICE」参照)
{ "type": "ice_candidate", "candidate": { "candidate": "...", "sdpMid": "0", "sdpMLineIndex": 0 } }

// keepalive
{ "type": "ping" }  →  { "type": "pong" }

// サーバー通知
{ "type": "peer_joined", "role": "device|admin" }
{ "type": "peer_left", "role": "device|admin" }
{ "type": "error", "message": "..." }
```

### シーケンス

1. device が起動時に一度だけ接続 (`role=device`)。以後は繋ぎっぱなし
   (「常時接続」— RTSP/PeerConnection はまだ起こさない)
2. admin が接続してくると (`role=admin`)、device は `peer_joined:{role:admin}`
   を受け取る。**このタイミングで初めて** RTSP pull + PeerConnection を
   起動し、`sdp_offer` を送る (admin 不在時に offer を送っても届け先が
   いないため無意味 — オフラインキューは無い)
3. admin は `sdp_answer` を返す。以後は signaling を経由せず直接 P2P で
   映像が流れる
4. admin が切断すると device は `peer_left:{role:admin}` を受け取り、
   RTSP/PeerConnection を畳んで signaling 接続だけの状態に戻る (次の
   admin 接続を待つ)
5. device 側の signaling WebSocket が切れたら「接続維持・再接続」節の
   バックオフで繋ぎ直す

## コーデック / メディア

- **映像のみ**。C212 の音声は PCM a-law で、admin 側ブラウザの
  WebRTC 実装は Opus 前提のため、v1 では音声トラックを一切含めない
- 映像は **H.264、RTP パススルー (無トランスコード)**。C212 の
  **stream2 (360p サブストリーム)** をそのまま転送する
  (ローカル表示用の stream1 とは別ストリーム・別 RTSP セッション)
- SDP の H.264 パラメータ (`profile-level-id` / `packetization-mode` 等) は
  カメラが返す値をそのまま使う。パススルーなので transcode できない

## ICE

- v1 は **STUN のみ**、TURN は使わない (admin ブラウザは拠点外の
  一般的なネットワークからの到達を想定)
- 既定 STUN: `stun:stun.l.google.com:19302` (両 device 実装で共通)
- **device 側は non-trickle** (ICE candidate 収集完了を待ってから
  1 通の SDP で送る)。esp_peer / pion どちらの実装も変更を最小化する
  設計判断。`ice_candidate` メッセージは device からは送信しない
- admin (ブラウザ) 側が trickle で `ice_candidate` を送ってくる可能性は
  プロトコル上残しているが、alc-gw (pion) は受信時に
  `pc.AddICECandidate` で追加する。alc-gw-p4 (esp_peer) は受信しても
  ログのみで無視する (esp_peer にトリックル ICE の注入 API が無いため)。
  TenkoRemoteAdminView 実装時は non-trickle (ICE 収集完了後に answer を
  送る) を推奨する

## 接続維持・再接続

- **signaling 接続は常時** (v1 はオンデマンド化しない。admin が
  いなくても device→signaling の WebSocket は保つ)
- **RTSP/PeerConnection は admin の出入りに連動** — 「メッセージ
  プロトコル」節のシーケンス参照。常時 RTSP pull していた WHIP 時代
  からの変更点
- signaling WebSocket の切断 (エラー・close 問わず) を検知したら
  **指数バックオフで再接続**: 1s → 2s → 4s → … → 60s 上限
  (両 device 実装で同じ範囲を使うこと)
- viewer (RTSP+PeerConnection) 単体の失敗 (peer failed / RTSP 切断) は
  signaling 接続自体を切らない。viewer を畳んで次の admin 接続を待つ
- 終了時 (アプリ終了 / OTA 再起動) は WebSocket を閉じるだけでよい
  (DO 側は `peer_left` を全ピアに配信して片付ける)

## 設定

### alc-gw (Windows 拠点)

`%LOCALAPPDATA%\alc-gw\config.json`:

```jsonc
{
  "rtsp_url":      "rtsp://user:pass@192.168.1.50/stream1",  // ローカル表示用
  "whip_rtsp_url": "rtsp://user:pass@192.168.1.50/stream2",  // 転送用 (360p)
  "whip_url":      "wss://<signaling>/cam-room/<拠点ID>",    // 旧: WHIP endpoint。中身の意味だけ変更
  "whip_token":    "<拠点トークン>"
}
```

キー名 (`whip_url`/`whip_token`) は歴史的経緯でそのまま維持している
(中身は WHIP endpoint ではなく signaling room の WebSocket URL)。
`internal/whip/session.go` のコメント参照。

環境変数 (開発時の上書き用、ファイルより優先):
`ALC_GW_WHIP_RTSP_URL` / `ALC_GW_WHIP_URL` / `ALC_GW_WHIP_TOKEN`

`whip_url` が空なら中継無効 (既存動作のまま、後方互換)。設定変更は
即座に反映され (トレイの「設定」→ config.json 編集 → 保存で再接続)、
SettingsDialog (UI) では扱わない — キッティング時に手で書く値のため

### alc-gw-p4 (Unit PoE-P4)

`idf.py menuconfig` の "alc-gw-p4 Relay Configuration"
(`CONFIG_RELAY_SIGNALING_URL` / `CONFIG_RELAY_SIGNALING_TOKEN` /
`CONFIG_RELAY_RTSP_URL` 等、v1 はビルド時埋め込み)。実運用デバイスが
まだ存在しないため、alc-gw と異なり `RELAY_WHIP_*` からの改名で
後方互換は取っていない。Android からのシリアル書き込みは Web Serial
非対応で実用不可なため、現場での設定変更はサーバー側配信を前提とし、
現場復旧は書き込み済み予備機の郵送交換で行う (OTA 2 スロット +
ロールバック必須)

## 実装リファレンス

- Go/pion 版 (本リポジトリ): [`internal/whip`](../internal/whip) —
  `signaling.go` (シグナリング: WebSocket dial/read/write) +
  `session.go` (`Session` が signaling 接続の常時維持、`viewer` が
  admin 1 名ぶんの RTSP producer 配線・PeerConnection のライフサイクル)。
  RTSP pull・pion 送信パイプライン (案 B: 素の pion +
  `TrackLocalStaticRTP`、go2rtc の `pkg/h264` で無トランスコードの
  まま 1200 バイト MTU に再パケタイズ) は WHIP 時代から変更なし
  ([`pkg/webrtc/consumer.go`](https://github.com/AlexxIT/go2rtc/blob/master/pkg/webrtc/consumer.go)
  と同じ変換チェーン)
- P4 ファーム版: [ippoan/alc-gw-p4](https://github.com/ippoan/alc-gw-p4) —
  `main/signaling_client.c` (esp_websocket_client + cJSON) +
  `main/relay.c` (`viewer_t` が admin 1 名ぶんのライフサイクル)。
  RTSP pull は `components/rtsp_client` (自前実装、alc-gw-p4#1)、
  `esp_peer_send_video()` で PeerConnection へ送出する部分は WHIP
  時代から変更なし
- シグナリングサーバー: [ippoan/alc-app](https://github.com/ippoan/alc-app)
  の `cf-alc-signaling/src/camera-signaling-room.ts`
  (`CameraSignalingRoom` Durable Object)

## v1 スコープ外 (将来検討)

- TURN
- 同時複数視聴 (真の SFU が要る用途が出てきたら再検討)
- 音声
- トークンの自動発行・失効 (ippoan/auth-worker#406 待ち)
- RTSP pull 自体の on-demand 化 (現状は admin 接続時に開始・切断時に
  停止するが、これはシグナリング層の話であって RTSP 接続確立の
  レイテンシそのものを短縮する話ではない。計測してから要否を判断)
