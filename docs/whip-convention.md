# WHIP publish 規約 (C212 全景配信)

遠隔点呼で C212 (Tapo) の全景映像を拠点外の管理者へ届けるための WHIP
publish 規約。全拠点「WHIP publish」にプロトコル統一する方針
([alc-gw#6](https://github.com/ippoan/alc-gw/issues/6)) のもと、publisher
実装は拠点構成で 2 種類に分かれるが、SFU から見た規約は完全に共通:

```
標準拠点:    C212 ─RTSP─▶ Unit PoE-P4 (ippoan/alc-gw-p4, esp_peer) ─WHIP─▶ SFU ─WHEP─▶ 管理者
Windows拠点: C212 ─RTSP─▶ alc-gw (本リポジトリ, internal/whip)     ─WHIP─▶ SFU ─WHEP─▶ 管理者
```

alc-gw の Go/pion 実装 (`internal/whip`) が P4 ファーム実装のリファレンス。
この文書の規約に従えばどちらの publisher も同じ SFU 設定 (エンドポイント・
トークン発行) で受けられる。

## エンドポイント

```
POST https://<sfu>/ingest/<拠点ID>
```

- 1 拠点 = 1 ストリーム = 1 URL。ストリーム ID は **拠点 ID そのもの**
  (点呼システム側の拠点 ID と同一の値を使う。新規の採番は行わない)
- WHIP (RFC 9725) 標準のネゴシエーションフロー:
  1. `POST` body = SDP offer (`Content-Type: application/sdp`)
  2. `201 Created` + `Location` ヘッダ (以後の DELETE 先、絶対 URL とは限らないため
     offer 先の URL を base にして解決すること) + body = SDP answer
  3. セッション終了時は `Location` へ `DELETE`
- Trickle ICE (`PATCH`) は v1 では使わない。ICE candidate は offer 生成前に
  収集を完了させてから POST する (non-trickle)

## 認証

- `Authorization: Bearer <拠点トークン>` を `POST` と `DELETE` の両方に付与
- トークンは拠点ごとに 1 本。v1 は配布・失効の自動化はせず、キッティング時に
  設定へ書き込む (下記「設定」参照)。将来的に P4 の「設定はサーバー側で行う」
  方針と統一する

## コーデック / メディア

- **映像のみ**。C212 の音声は PCM a-law、SFU (Cloudflare Realtime 想定) は
  Opus 前提のため、v1 では音声トラックを一切含めない
- 映像は **H.264、RTP パススルー (無トランスコード)**。C212 の
  **stream2 (360p サブストリーム)** をそのまま WHIP へ転送する
  (ローカル表示用の stream1 とは別ストリーム・別 RTSP セッション)
- SDP の H.264 パラメータ (`profile-level-id` / `packetization-mode` 等) は
  カメラが返す値をそのまま使う。SFU 側で特定プロファイルを要求しない
  (パススルーなので transcode できない)

## ICE

- v1 は **STUN のみ**、TURN は使わない (SFU は公衆到達可能なサーバーを想定)
- 既定 STUN: `stun:stun.l.google.com:19302` (双方の publisher 実装で共通)
- ICE candidate は host / STUN 由来の srflx のみで、LAN 限定フィルタ
  (ローカル WHEP 配信用の `internal/stream` が使うもの) とは無関係の別経路

## 接続維持・再接続

- **常時 publish** (v1 はオンデマンド化しない。視聴者ゼロでも接続を保つ)
- 切断 (RTSP 切断・PeerConnection failed/disconnected・SFU 側切断) を検知
  したら **指数バックオフで再接続**: 1s → 2s → 4s → … → 60s 上限
  (両 publisher 実装で同じ範囲を使うこと。P4 側は OTA 中の再接続嵐を避ける
  意味でも上限を守る)
- 再接続のたびに新しい WHIP セッション (新規 `POST`) を張り直す。古い
  `Location` への `DELETE` は best-effort (タイムアウト等で失敗しても
  再接続は続行してよい)
- 終了時 (アプリ終了 / OTA 再起動) は `DELETE` を送ってから停止する
  (SFU 側にゾンビセッションを残さない)

## 設定

### alc-gw (Windows 拠点)

`%LOCALAPPDATA%\alc-gw\config.json`:

```jsonc
{
  "rtsp_url":      "rtsp://user:pass@192.168.1.50/stream1",  // ローカル表示用
  "whip_rtsp_url": "rtsp://user:pass@192.168.1.50/stream2",  // 転送用 (360p)
  "whip_url":      "https://<sfu>/ingest/<拠点ID>",
  "whip_token":    "<拠点トークン>"
}
```

環境変数 (開発時の上書き用、ファイルより優先):
`ALC_GW_WHIP_RTSP_URL` / `ALC_GW_WHIP_URL` / `ALC_GW_WHIP_TOKEN`

`whip_url` が空なら publish 無効 (既存動作のまま、後方互換)。設定変更は
即座に反映され (トレイの「設定」→ config.json 編集 → 保存で再接続)、
SettingsDialog (UI) では扱わない — キッティング時に手で書く値のため

### alc-gw-p4 (Unit PoE-P4)

サーバー側で配布する設定に上記と同じ 3 値
(WHIP エンドポイント・拠点トークン・転送用 RTSP URL) を持たせる。
Android からのシリアル書き込みは Web Serial 非対応で実用不可なため、
現場での設定変更はサーバー側配信を前提とし、現場復旧は書き込み済み予備機
の郵送交換で行う (OTA 2 スロット + ロールバック必須)

## 実装リファレンス

- Go/pion 版 (本リポジトリ): [`internal/whip`](../internal/whip) —
  `Client` (シグナリング: POST/DELETE) + `Session` (RTSP producer 配線・
  再接続ループ)。案 B (素の pion + `TrackLocalStaticRTP`) を採用、
  go2rtc の `pkg/h264` (`RTPDepay`/`RTPPay`) で無トランスコードのまま
  1200 バイト MTU に再パケタイズしている
  ([`pkg/webrtc/consumer.go`](https://github.com/AlexxIT/go2rtc/blob/master/pkg/webrtc/consumer.go)
  と同じ変換チェーン)
- P4 ファーム版: [ippoan/alc-gw-p4#1](https://github.com/ippoan/alc-gw-p4/issues/1) —
  `esp_media_protocols` で RTSP pull、`esp_peer_send_video()` で WHIP publish
  (`esp_peer` の `whip_demo` が参考実装)

## v1 スコープ外 (将来検討)

- Trickle ICE / TURN
- オンデマンド publish (点呼セッション連動)
- 音声
- トークンの自動発行・失効
