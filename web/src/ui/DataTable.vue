<script setup lang="ts">
import { Table, type TableColumn } from '@asterism-dev/lumen'

defineProps<{ columns: TableColumn[]; rows: Record<string, unknown>[]; rowKey: string }>()
</script>

<template>
  <div class="data-table">
    <Table :columns="columns" :rows="rows" :row-key="rowKey" v-bind="$attrs">
      <template v-for="(_, name) in $slots" #[name]="slotProps"><slot :name="name" v-bind="slotProps ?? {}" /></template>
    </Table>
  </div>
</template>

<style scoped>
.data-table {
  overflow: hidden;
  border: 1px solid var(--border);
  border-radius: calc(var(--radius) + 2px);
  background: var(--panel);
}
.data-table :deep(.lm-table__th) {
  padding: 10px 16px;
  font-family: var(--font-mono);
  font-size: 0.7rem;
  font-weight: normal;
  letter-spacing: 0.08em;
  text-transform: uppercase;
}
.data-table :deep(.lm-table__td) {
  padding: 10px 16px;
}
.data-table :deep(tbody tr:last-child .lm-table__td) {
  border-bottom: 0;
}
</style>
