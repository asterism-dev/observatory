<script setup lang="ts">
import CopyButton from './CopyButton.vue'

defineProps<{ command: string; prompt?: boolean }>()
</script>

<template>
  <div class="command">
    <code><span v-if="prompt !== false" class="muted">$ </span>{{ command }}</code>
    <CopyButton :text="command" label />
  </div>
</template>

<style scoped>
.command {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
  min-width: 0;
  padding: 8px 8px 8px 14px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--bg);
}
code {
  font-family: var(--font-mono);
  overflow-wrap: anywhere;
}
</style>
