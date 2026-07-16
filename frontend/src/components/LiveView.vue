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
    pc.ontrack = (ev) => {
      if (video.value && ev.streams[0]) {
        video.value.srcObject = ev.streams[0]
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
    <video ref="video" autoplay muted playsinline></video>
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

video {
  width: 100%;
  max-height: 70vh;
  background: #000;
  border-radius: 8px;
}

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
