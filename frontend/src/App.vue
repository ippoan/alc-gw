<script lang="ts" setup>
import {onMounted} from 'vue'

// WebView のデフォルト表示は点呼UI (alc.ippoan.org)
const HOME_URL = 'https://alc.ippoan.org/'

onMounted(() => {
  location.replace(HOME_URL)
})
</script>

<template>
  <main></main>
</template>
