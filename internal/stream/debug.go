package stream

import "net/http"

// DebugPage は WHEP 再生 + WebRTC 統計表示のスタンドアロンページ。
// 「接続はできるが映像が出ない」の切り分け用に、WebView の外
// (通常ブラウザ) から http://127.0.0.1:11984/debug で開ける。
func DebugPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(debugHTML))
}

const debugHTML = `<!DOCTYPE html>
<html lang="ja">
<head>
<meta charset="utf-8">
<title>alc-gw stream debug</title>
<style>
body { font-family: monospace; background: #111; color: #ddd; padding: 16px; }
video { width: 640px; background: #000; display: block; margin-bottom: 12px; }
pre { background: #1e1e1e; padding: 8px; white-space: pre-wrap; }
button { padding: 6px 16px; margin-right: 8px; }
</style>
</head>
<body>
<h1>alc-gw stream debug</h1>
<video id="v" autoplay muted playsinline></video>
<div>
  <button onclick="start()">start</button>
  <button onclick="stop()">stop</button>
</div>
<pre id="log"></pre>
<pre id="stats"></pre>
<script>
let pc, timer;
const log = (m) => { document.getElementById('log').textContent += m + '\n'; };

async function start() {
  stop();
  pc = new RTCPeerConnection();
  pc.addTransceiver('video', {direction: 'recvonly'});
  pc.addTransceiver('audio', {direction: 'recvonly'});
  const stream = new MediaStream();
  pc.ontrack = (ev) => {
    log('ontrack: kind=' + ev.track.kind + ' id=' + ev.track.id + ' streams=' + ev.streams.length);
    stream.addTrack(ev.track);
    document.getElementById('v').srcObject = stream;
  };
  pc.onconnectionstatechange = () => log('connectionState: ' + pc.connectionState);

  const offer = await pc.createOffer();
  await pc.setLocalDescription(offer);
  await new Promise((res) => {
    if (pc.iceGatheringState === 'complete') return res();
    pc.addEventListener('icegatheringstatechange', () => {
      if (pc.iceGatheringState === 'complete') res();
    });
  });

  const r = await fetch('/api/whep', {method: 'POST', headers: {'Content-Type': 'application/sdp'}, body: pc.localDescription.sdp});
  log('whep: ' + r.status);
  if (!r.ok) { log(await r.text()); return; }
  await pc.setRemoteDescription({type: 'answer', sdp: await r.text()});
  log('answer set');

  timer = setInterval(showStats, 1000);
}

async function showStats() {
  if (!pc) return;
  const out = [];
  const v = document.getElementById('v');
  out.push('video element: readyState=' + v.readyState + ' videoWidth=' + v.videoWidth + ' videoHeight=' + v.videoHeight + ' paused=' + v.paused);
  const stats = await pc.getStats();
  stats.forEach((s) => {
    if (s.type === 'inbound-rtp') {
      out.push('inbound-rtp ' + s.kind + ': bytes=' + s.bytesReceived + ' packets=' + s.packetsReceived +
        ' framesDecoded=' + (s.framesDecoded ?? '-') + ' framesDropped=' + (s.framesDropped ?? '-') +
        ' keyFramesDecoded=' + (s.keyFramesDecoded ?? '-') +
        ' ' + (s.frameWidth ?? '?') + 'x' + (s.frameHeight ?? '?') +
        ' pliCount=' + (s.pliCount ?? '-') + ' nackCount=' + (s.nackCount ?? '-') +
        ' codec=' + (s.codecId ?? '?'));
    }
    if (s.type === 'codec') {
      out.push('codec ' + s.id + ': ' + s.mimeType + ' ' + (s.sdpFmtpLine ?? ''));
    }
  });
  document.getElementById('stats').textContent = out.join('\n');
}

function stop() {
  if (timer) clearInterval(timer);
  if (pc) pc.close();
  pc = undefined;
  document.getElementById('v').srcObject = null;
}
</script>
</body>
</html>
`
