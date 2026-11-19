<script setup lang="ts">
import CommandBox from './CommandBox.vue'

defineProps<{ label?: string; title: string; text?: string; command?: string }>()
</script>

<template>
  <div class="empty" :class="{ 'empty--split': command }">
    <div class="empty__main">
      <span v-if="label" class="empty__label">{{ label }}</span>
      <h2>{{ title }}</h2>
      <p v-if="text">{{ text }}</p>
      <div v-if="$slots.default" class="empty__actions"><slot /></div>
    </div>
    <div v-if="command" class="empty__side">
      <span class="empty__label empty__label--plain">CLI</span>
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
.empty__label {
  display: flex;
  align-items: center;
  gap: 8px;
  font-family: var(--font-mono);
  font-size: 0.7rem;
  letter-spacing: 0.08em;
  text-transform: uppercase;
  color: var(--muted);
}
.empty__label:not(.empty__label--plain)::before {
  content: '';
  width: 5px;
  height: 5px;
  background: var(--accent);
  transform: rotate(45deg);
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
