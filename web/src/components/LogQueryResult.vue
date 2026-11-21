<script setup lang="ts">
import type { TableColumn } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import type { LogQueryResult } from '@/composables/logs'
import { Card, DataTable } from '@/ui'

const props = defineProps<{ result: LogQueryResult }>()
const { t } = useI18n()
const open = ref(new Set<number>())

const statRows = computed(() => props.result.rows.map((r, i) => ({ ...r, __row: i })))
watch(() => props.result, () => (open.value = new Set()))
const columns = computed<TableColumn[]>(() => props.result.columns.map((c) => ({ key: c, label: c })))
const toggle = (i: number) => (open.value.delete(i) ? null : open.value.add(i))
</script>

<template>
  <DataTable v-if="result.stats" :columns="columns" :rows="statRows" row-key="__row">
    <template #empty>{{ t('logs.noRows') }}</template>
  </DataTable>
  <Card v-else>
    <div class="lines mono">
      <p v-if="!result.rows.length" class="muted">{{ t('logs.noRows') }}</p>
      <template v-for="(r, i) in result.rows" :key="i">
        <div class="ln" role="button" tabindex="0" :aria-expanded="open.has(i)" @click="toggle(i)" @keydown.enter="toggle(i)">
          <span class="muted">{{ open.has(i) ? '▾' : '▸' }} {{ r._time ?? '' }}</span>
          <span class="msg">{{ r._msg }}</span>
        </div>
        <pre v-if="open.has(i)" class="json">{{ JSON.stringify(r, null, 2) }}</pre>
      </template>
    </div>
  </Card>
</template>

<style scoped>
.lines {
  max-height: 480px;
  overflow: auto;
  font-size: 0.72rem;
}
.ln {
  display: grid;
  grid-template-columns: 210px 1fr;
  gap: 8px;
  padding: 1px 4px;
  cursor: pointer;
}
.ln:hover {
  background: var(--select);
}
.msg {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.json {
  margin: 2px 4px 6px 4px;
  padding: 8px 10px;
  border-radius: var(--radius);
  background: var(--select);
  overflow: auto;
}
p {
  margin: 0;
}
</style>
