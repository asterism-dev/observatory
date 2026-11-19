<script setup lang="ts">
defineProps<{ tone?: 'danger'; flush?: boolean }>()
</script>

<template>
  <div class="card" :class="[tone && `card--${tone}`, { 'card--flush': flush }]"><slot /></div>
</template>

<style scoped>
.card {
  display: grid;
  gap: 10px;
  min-width: 0;
  padding: 14px 18px;
  border: 1px solid var(--border);
  border-radius: calc(var(--radius) + 2px);
  background: var(--panel);
}
.card--flush {
  padding: 0;
  overflow: hidden;
}
.card--danger {
  border-color: var(--danger);
}
.card > :deep(p) {
  margin: 0;
}
</style>
