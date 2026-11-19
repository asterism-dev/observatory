<script setup lang="ts">
defineProps<{ empty?: string; count: number }>()
</script>

<template>
  <ul class="resource-list">
    <slot />
    <li v-if="!count" class="resource-list__empty"><slot name="empty">{{ empty }}</slot></li>
  </ul>
</template>

<style scoped>
.resource-list {
  display: grid;
  gap: 8px;
  margin: 0;
  padding: 0;
  list-style: none;
}
.resource-list__empty {
  padding: 28px;
  border: 1px dashed var(--border);
  border-radius: calc(var(--radius) + 2px);
  color: var(--muted);
  text-align: center;
}
</style>
