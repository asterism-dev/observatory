<script setup lang="ts">
import { BookOpen } from '@lucide/vue'
import type { Component } from 'vue'
import { useI18n } from 'vue-i18n'

import { useMe } from '@/composables/useMe'

defineProps<{ icon?: Component; title: string; subtitle?: string; mono?: boolean }>()
const { t } = useI18n()
const { me } = useMe()
</script>

<template>
  <header class="page-header">
    <span v-if="icon" class="page-header__icon"><component :is="icon" :size="20" :stroke-width="1.75" /></span>
    <div class="page-header__text">
      <div class="page-header__title">
        <h1 :class="{ mono }">{{ title }}</h1>
        <slot name="tags" />
      </div>
      <p v-if="subtitle">{{ subtitle }}</p>
    </div>
    <div class="page-header__actions">
      <a v-if="me?.docsURL" :href="me.docsURL" target="_blank" rel="noopener" class="lm-button"><BookOpen :size="16" :stroke-width="1.75" />{{ t('nav.docs') }}</a>
      <slot />
    </div>
  </header>
</template>

<style scoped>
.page-header {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: var(--page-y) var(--page-x);
  border-bottom: 1px solid var(--border);
}
.page-header__icon {
  display: grid;
  place-items: center;
  flex: none;
  width: 40px;
  height: 40px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
}
.page-header__text {
  flex: 1;
  min-width: 0;
}
.page-header__title {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
h1 {
  margin: 0;
  font-size: 1.25rem;
  font-weight: 600;
}
p {
  margin: 2px 0 0;
  color: var(--muted);
}
.page-header__actions {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
.page-header__actions a {
  color: var(--text);
  text-decoration: none;
}
@media (max-width: 640px) {
  .page-header {
    flex-wrap: wrap;
  }
}
</style>
