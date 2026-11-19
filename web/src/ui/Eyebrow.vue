<script setup lang="ts">
defineProps<{ as?: string; accent?: boolean; plain?: boolean }>()
</script>

<template>
  <component :is="as ?? 'span'" class="eyebrow" :class="{ 'eyebrow--accent': accent, 'eyebrow--plain': plain }"><slot /></component>
</template>

<style scoped>
.eyebrow {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 0;
  font-family: var(--font-mono);
  font-size: 0.7rem;
  font-weight: normal;
  letter-spacing: 0.08em;
  text-transform: uppercase;
  color: var(--muted);
}
.eyebrow:not(.eyebrow--plain)::before {
  content: '';
  width: 5px;
  height: 5px;
  background: currentColor;
  transform: rotate(45deg);
}
.eyebrow--accent::before {
  background: var(--accent);
}
</style>
