<script setup lang="ts">
import { Tabs } from '@asterism-dev/lumen'
import { computed, type Component } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import type { NavTab } from '@/composables/useSettingsNav'
import Page from '@/ui/Page.vue'

const props = defineProps<{ icon: Component; title: string; subtitle: string; tabs: NavTab[] }>()
const route = useRoute()
const router = useRouter()

// Detail pages like /orgs/:id/teams/:teamId keep their parent tab active.
const active = computed({
  get: () => props.tabs.find((tab) => route.path === tab.path || route.path.startsWith(tab.path + '/'))?.path ?? '',
  set: (path) => router.push(path),
})
</script>

<template>
  <Page :icon="icon" :title="title" :subtitle="subtitle">
    <Tabs v-model="active" :tabs="tabs.map((tab) => ({ value: tab.path, label: tab.label }))" variant="underline" activation-mode="manual" :aria-label="title" />
    <RouterView />
  </Page>
</template>
