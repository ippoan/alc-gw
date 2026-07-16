<script lang="ts" setup>
import {onUnmounted, ref} from 'vue'

// WHEP (WebRTC-HTTP Egress Protocol) でカメラ映像を受信する。
// エンドポイントは Wails AssetServer 上の同一オリジン /api/whep。
const video = ref<HTMLVideoElement>()
const status = ref<'idle' | 'connecting' | 'connected' | 'error'>('idle')
const errorMessage = ref('')

let pc: RTCPeerConnection | undefined

async function start() {
  stop()
  status.value = 'connecting'
  errorMessage.value = ''

  try {
    // LAN 内完結のため ICE サーバーなし (host candidate のみ)
    pc = new RTCPeerConnection()
    pc.addTransceiver('video', {direction: 'recvonly'})
    pc.addTransceiver('audio', {direction: 'recvonly'})
    // answer SDP に msid が無いと ev.streams が空になるため、
    // 受信トラックを自前の MediaStream に集約する
    const stream = new MediaStream()
    pc.ontrack = (ev) => {
      stream.addTrack(ev.track)
      if (video.value && video.value.srcObject !== stream) {
        video.value.srcObject = stream
      }
    }
    pc.onconnectionstatechange = () => {
      if (!pc) return
      if (pc.connectionState === 'connected') {
        status.value = 'connected'
      } else if (pc.connectionState === 'failed') {
        status.value = 'error'
        errorMessage.value = 'WebRTC 接続に失敗しました'
      }
    }

    const offer = await pc.createOffer()
    await pc.setLocalDescription(offer)
    await waitIceGathering(pc)

    const res = await fetch('/api/whep', {
      method: 'POST',
      headers: {'Content-Type': 'application/sdp'},
      body: pc.localDescription!.sdp,
    })
    if (!res.ok) {
      throw new Error(`${res.status}: ${await res.text()}`)
    }
    await pc.setRemoteDescription({type: 'answer', sdp: await res.text()})
  } catch (err) {
    status.value = 'error'
    errorMessage.value = String(err)
    stop()
  }
}

function stop() {
  pc?.close()
  pc = undefined
  if (video.value) video.value.srcObject = null
  if (status.value !== 'error') status.value = 'idle'
}

// --- パンチルト (ONVIF PTZ) ---
// 押している間 ContinuousMove、離すと Stop
const PTZ_SPEED = 0.5

async function ptzMove(x: number, y: number) {
  try {
    await fetch('/api/ptz', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({x, y}),
    })
  } catch {
    // 移動失敗は致命ではないので黙殺 (次の操作で回復を試みる)
  }
}

function ptzStop() {
  void ptzMove(0, 0)
}

function waitIceGathering(pc: RTCPeerConnection): Promise<void> {
  if (pc.iceGatheringState === 'complete') return Promise.resolve()
  return new Promise((resolve) => {
    const check = () => {
      if (pc.iceGatheringState === 'complete') {
        pc.removeEventListener('icegatheringstatechange', check)
        resolve()
      }
    }
    pc.addEventListener('icegatheringstatechange', check)
  })
}

onUnmounted(stop)
</script>

<template>
  <div class="live-view">
    <div class="video-wrap">
      <video ref="video" autoplay muted playsinline></video>
      <div class="ptz-pad" v-show="status === 'connected'">
        <button class="ptz up" aria-label="上"
                @pointerdown="ptzMove(0, PTZ_SPEED)" @pointerup="ptzStop" @pointerleave="ptzStop" @pointercancel="ptzStop">▲</button>
        <button class="ptz left" aria-label="左"
                @pointerdown="ptzMove(-PTZ_SPEED, 0)" @pointerup="ptzStop" @pointerleave="ptzStop" @pointercancel="ptzStop">◀</button>
        <button class="ptz right" aria-label="右"
                @pointerdown="ptzMove(PTZ_SPEED, 0)" @pointerup="ptzStop" @pointerleave="ptzStop" @pointercancel="ptzStop">▶</button>
        <button class="ptz down" aria-label="下"
                @pointerdown="ptzMove(0, -PTZ_SPEED)" @pointerup="ptzStop" @pointerleave="ptzStop" @pointercancel="ptzStop">▼</button>
      </div>
    </div>
    <div class="controls">
      <button :disabled="status === 'connecting'" @click="start">
        {{ status === 'connected' ? '再接続' : '映像開始' }}
      </button>
      <button :disabled="status === 'idle'" @click="stop">停止</button>
      <span class="status" :data-status="status">
        {{
          status === 'idle' ? '待機中'
          : status === 'connecting' ? '接続中…'
          : status === 'connected' ? '受信中'
          : errorMessage
        }}
      </span>
    </div>
  </div>
</template>

<style scoped>
.live-view {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 16px;
}

.video-wrap {
  position: relative;
}

video {
  width: 100%;
  max-height: 70vh;
  background: #000;
  border-radius: 8px;
  display: block;
}

/* タッチ画面での押しっぱなし操作を想定した方向パッド */
.ptz-pad {
  position: absolute;
  right: 16px;
  bottom: 16px;
  display: grid;
  grid-template-areas:
    ". up ."
    "left . right"
    ". down .";
  gap: 4px;
}

.ptz {
  width: 48px;
  height: 48px;
  padding: 0;
  font-size: 18px;
  border: none;
  border-radius: 8px;
  background: rgba(30, 41, 59, 0.75);
  color: #fff;
  cursor: pointer;
  touch-action: none;
  user-select: none;
}

.ptz:active {
  background: rgba(37, 99, 235, 0.9);
}

.ptz.up { grid-area: up; }
.ptz.left { grid-area: left; }
.ptz.right { grid-area: right; }
.ptz.down { grid-area: down; }

.controls {
  display: flex;
  align-items: center;
  gap: 12px;
}

button {
  padding: 8px 20px;
  font-size: 16px;
  border: none;
  border-radius: 6px;
  background: #2563eb;
  color: #fff;
  cursor: pointer;
}

button:disabled {
  opacity: 0.5;
  cursor: default;
}

.status[data-status='error'] {
  color: #f87171;
}

.status[data-status='connected'] {
  color: #4ade80;
}
</style>
