<script setup lang="ts">
import { BookOpen } from '@lucide/vue'
import type { Component } from 'vue'
import { useI18n } from 'vue-i18n'

import { useMe } from '@/composables/useMe'

defineProps<{ icon: Component; title: string; subtitle: string }>()
const { t } = useI18n()
const { me } = useMe()
</script>

<template>
  <header class="page-head">
    <span class="icon"><component :is="icon" :size="20" :stroke-width="1.75" /></span>
    <div class="text">
      <h1>{{ title }}</h1>
      <p>{{ subtitle }}</p>
    </div>
    <div class="actions">
      <a v-if="me?.docsURL" :href="me.docsURL" target="_blank" rel="noopener" class="docs"><BookOpen :size="16" :stroke-width="1.75" />{{ t('nav.docs') }}</a>
      <slot />
    </div>
  </header>
</template>

<style scoped>
.page-head {
  display: flex;
  align-items: center;
  gap: 12px;
  padding-bottom: 20px;
  margin-bottom: 20px;
  border-bottom: 1px solid var(--border);
}
.icon {
  display: grid;
  place-items: center;
  flex: none;
  width: 40px;
  height: 40px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
}
.text {
  flex: 1;
  min-width: 0;
}
h1 {
  margin: 0;
  font-size: 1.25rem;
}
p {
  margin: 2px 0 0;
  color: var(--muted);
}
.actions {
  display: flex;
  align-items: center;
  gap: 8px;
}
.docs {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 6px 12px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  color: var(--text);
  text-decoration: none;
}
.docs:hover {
  border-color: var(--accent);
}
@media (max-width: 640px) {
  .page-head {
    flex-wrap: wrap;
  }
}
</style>
