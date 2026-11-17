<script setup lang="ts">
import { useRoute } from 'vue-router'

import AppShell from '@/components/AppShell.vue'

const route = useRoute()
</script>

<template>
  <RouterView v-if="route.meta.public" />
  <AppShell v-else><RouterView /></AppShell>
</template>

<style>
html,
body,
#app {
  height: 100%;
  margin: 0;
}
body {
  background: var(--bg);
  color: var(--text);
  font-family: var(--font-ui);
}
</style>
