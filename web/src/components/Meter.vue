<script setup lang="ts">
import { computed } from 'vue'

const props = defineProps<{ label: string; percent: number | null }>()
const SEGMENTS = 10
const tone = computed(() => ((props.percent ?? 0) >= 90 ? 'danger' : (props.percent ?? 0) >= 70 ? 'warning' : 'success'))
const lit = computed(() => (props.percent === null ? 0 : Math.ceil((Math.min(Math.max(props.percent, 0), 100) / 100) * SEGMENTS)))
</script>

<template>
  <span class="meter" role="meter" :aria-label="label" :aria-valuenow="percent ?? undefined" aria-valuemin="0" aria-valuemax="100">
    <span class="label">{{ label }}</span>
    <span class="cells" :class="tone"><span v-for="i in SEGMENTS" :key="i" :class="{ on: i <= lit }" /></span>
    <span class="value">{{ percent === null ? '–' : `${Math.round(percent)}%` }}</span>
  </span>
</template>

<style scoped>
.meter {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  font-family: var(--font-mono, ui-monospace, monospace);
  font-size: 0.7rem;
  color: var(--muted);
}
.cells {
  display: inline-flex;
  gap: 2px;
}
.cells > span {
  width: 5px;
  height: 14px;
  border-radius: 1px;
  background: var(--border);
}
.success > span.on {
  background: var(--success);
}
.warning > span.on {
  background: var(--warning);
}
.danger > span.on {
  background: var(--danger);
}
.value {
  min-width: 3ch;
  color: var(--text);
}
</style>
