<script setup lang="ts">
defineProps<{ tone?: 'muted' | 'success' | 'warning' | 'danger' }>()
</script>

<template>
  <span class="dot" :class="`dot--${tone ?? 'muted'}`" aria-hidden="true" />
</template>

<style scoped>
.dot {
  flex: none;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--muted);
}
.dot--success {
  background: var(--success);
  box-shadow: 0 0 0 3px color-mix(in srgb, var(--success) 20%, transparent);
}
.dot--warning {
  background: var(--warning);
}
.dot--danger {
  background: var(--danger);
}
</style>
