<script lang="ts" setup>
import {onMounted, ref} from 'vue'
import {EventsOn} from '../../wailsjs/runtime/runtime'
import {GetSettings, SaveSettings} from '../../wailsjs/go/main/App'

const open = ref(false)
const rtspUrl = ref('')
const version = ref('')
const message = ref('')

async function load() {
  const s = await GetSettings()
  rtspUrl.value = s.rtspUrl
  version.value = s.version
}

async function show() {
  message.value = ''
  await load()
  open.value = true
}

async function save() {
  message.value = ''
  try {
    await SaveSettings(rtspUrl.value.trim())
    message.value = '保存しました。映像を再接続してください。'
  } catch (err) {
    message.value = '保存に失敗しました: ' + String(err)
  }
}

onMounted(() => {
  EventsOn('open-settings', show)
})

defineExpose({show})
</script>

<template>
  <div v-if="open" class="overlay" @click.self="open = false">
    <div class="dialog">
      <h2>設定</h2>
      <label>
        カメラ RTSP URL
        <input v-model="rtspUrl" type="text"
               placeholder="rtsp://user:pass@192.168.x.x:554/stream1"
               spellcheck="false"/>
      </label>
      <p class="hint">
        Tapo はアプリの「高度な設定 → カメラのアカウント」で作成した
        ユーザー名・パスワードを使います。パンチルトも同じ認証情報で動きます。
      </p>
      <p v-if="message" class="message">{{ message }}</p>
      <div class="actions">
        <span class="version">alc-gw {{ version }}</span>
        <button class="secondary" @click="open = false">閉じる</button>
        <button @click="save">保存</button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.overlay {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.5);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 100;
}

.dialog {
  background: #1e293b;
  border-radius: 12px;
  padding: 24px;
  width: min(560px, 90vw);
  display: flex;
  flex-direction: column;
  gap: 12px;
}

h2 {
  font-size: 18px;
  margin: 0;
}

label {
  display: flex;
  flex-direction: column;
  gap: 6px;
  font-size: 14px;
}

input {
  padding: 10px;
  font-size: 14px;
  border: 1px solid #475569;
  border-radius: 6px;
  background: #0f172a;
  color: #e2e8f0;
}

.hint {
  font-size: 12px;
  color: #94a3b8;
  margin: 0;
}

.message {
  font-size: 13px;
  color: #4ade80;
  margin: 0;
}

.actions {
  display: flex;
  align-items: center;
  gap: 8px;
  justify-content: flex-end;
}

.version {
  margin-right: auto;
  font-size: 12px;
  color: #64748b;
}

button {
  padding: 8px 20px;
  font-size: 14px;
  border: none;
  border-radius: 6px;
  background: #2563eb;
  color: #fff;
  cursor: pointer;
}

button.secondary {
  background: #475569;
}
</style>
