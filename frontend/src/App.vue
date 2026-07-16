<script lang="ts" setup>
import LiveView from './components/LiveView.vue'
import SettingsDialog from './components/SettingsDialog.vue'
</script>

<template>
  <main>
    <h1>alc-gw</h1>
    <LiveView/>
    <SettingsDialog/>
  </main>
</template>

<style>
main {
  max-width: 960px;
  margin: 0 auto;
  padding: 24px;
}

h1 {
  font-size: 20px;
  margin-bottom: 8px;
}
</style>
