<script setup lang="ts">
import { Tabs } from '@asterism-dev/lumen'
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import type { NavTab } from '@/composables/useSettingsNav'

const props = defineProps<{ title: string; tabs: NavTab[] }>()
const route = useRoute()
const router = useRouter()

// Detail pages like /orgs/:id/teams/:teamId keep their parent tab active.
const active = computed({
  get: () => props.tabs.find((tab) => route.path === tab.path || route.path.startsWith(tab.path + '/'))?.path ?? '',
  set: (path) => router.push(path),
})
</script>

<template>
  <h1>{{ title }}</h1>
  <Tabs v-model="active" :tabs="tabs.map((tab) => ({ value: tab.path, label: tab.label }))" variant="underline" :aria-label="title" />
  <div class="settings__body"><RouterView /></div>
</template>

<style scoped>
.settings__body {
  padding-top: 16px;
}
</style>
