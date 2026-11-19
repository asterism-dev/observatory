<script setup lang="ts">
import { useRouter } from 'vue-router'

import StatusDot from './StatusDot.vue'

const props = defineProps<{ title: string; meta?: string; to?: string; tone?: 'muted' | 'success' | 'warning' | 'danger' }>()
const router = useRouter()
const open = () => props.to && router.push(props.to)
</script>

<template>
  <li class="row" :class="{ 'row--link': to }" @click="open">
    <StatusDot v-if="tone" :tone="tone" />
    <div class="row__ident">
      <router-link v-if="to" :to="to" class="row__title" @click.stop>{{ title }}</router-link>
      <span v-else class="row__title">{{ title }}</span>
      <span v-if="meta || $slots.meta" class="row__meta"><slot name="meta">{{ meta }}</slot></span>
    </div>
    <span v-if="$slots.tags" class="row__tags"><slot name="tags" /></span>
    <span class="row__details"><slot /></span>
    <span v-if="$slots.actions" class="row__actions" @click.stop><slot name="actions" /></span>
  </li>
</template>

<style scoped>
.row {
  display: grid;
  grid-template-columns: auto minmax(160px, 1fr) auto minmax(0, 2fr) auto;
  align-items: center;
  gap: 16px;
  padding: 14px 18px;
  border: 1px solid var(--border);
  border-radius: calc(var(--radius) + 2px);
  background: var(--panel);
}
.row:not(:has(> .dot)) {
  grid-template-columns: minmax(160px, 1fr) auto minmax(0, 2fr) auto;
}
.row--link {
  cursor: pointer;
}
.row--link:hover {
  border-color: color-mix(in srgb, var(--accent) 60%, var(--border));
}
.row__ident {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}
.row__title {
  overflow: hidden;
  color: var(--text);
  font-family: var(--font-mono);
  font-weight: 600;
  text-decoration: none;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.row__meta {
  color: var(--muted);
  font-family: var(--font-mono);
  font-size: 0.72rem;
}
.row__tags,
.row__details,
.row__actions {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px 16px;
  min-width: 0;
}
.row__tags {
  gap: 6px;
}
.row__details {
  color: var(--muted);
}
.row__actions {
  gap: 4px;
  justify-content: flex-end;
  grid-column: -2;
}
@media (max-width: 900px) {
  .row,
  .row:not(:has(> .dot)) {
    grid-template-columns: auto minmax(0, 1fr);
  }
  .row > :not(.dot):not(.row__ident) {
    grid-column: 2;
    justify-content: flex-start;
  }
  .row:not(:has(> .dot)) > * {
    grid-column: 1 / -1;
  }
}
</style>
