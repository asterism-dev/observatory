<script setup lang="ts">
import CommandBox from './CommandBox.vue'
import Eyebrow from './Eyebrow.vue'

defineProps<{ label?: string; title: string; text?: string; command?: string }>()
</script>

<template>
  <div class="empty" :class="{ 'empty--split': command }">
    <div class="empty__main">
      <Eyebrow v-if="label" accent>{{ label }}</Eyebrow>
      <h2>{{ title }}</h2>
      <p v-if="text">{{ text }}</p>
      <div v-if="$slots.default" class="empty__actions"><slot /></div>
    </div>
    <div v-if="command" class="empty__side">
      <Eyebrow plain>CLI</Eyebrow>
      <CommandBox :command="command" />
    </div>
  </div>
</template>

<style scoped>
.empty {
  display: grid;
  border: 1px solid var(--border);
  border-radius: calc(var(--radius) + 2px);
  background: var(--panel);
}
.empty--split {
  grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
}
.empty__main {
  display: grid;
  align-content: center;
  gap: 8px;
  padding: 40px;
}
.empty__side {
  display: grid;
  align-content: start;
  gap: 12px;
  padding: 20px;
  border-left: 1px solid var(--border);
}
h2 {
  margin: 0;
  font-size: 1.4rem;
}
p {
  margin: 0;
  color: var(--muted);
}
.empty__actions {
  display: flex;
  gap: 8px;
  margin-top: 8px;
}
@media (max-width: 640px) {
  .empty__main {
    padding: 24px;
  }
  .empty__side {
    border-left: 0;
    border-top: 1px solid var(--border);
  }
}
</style>
