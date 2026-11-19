<script setup lang="ts">
defineProps<{ tone?: 'default' | 'muted' | 'success' | 'warning' | 'danger'; strong?: boolean }>()
</script>

<template>
  <span class="tag" :class="[`tag--${tone ?? 'default'}`, { 'tag--strong': strong }]"><slot /></span>
</template>

<style scoped>
.tag {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 1px 6px;
  border: 1px solid var(--border);
  border-radius: 4px;
  font-family: var(--font-mono);
  font-size: 0.65rem;
  letter-spacing: 0.06em;
  line-height: 1.5;
  text-transform: uppercase;
  white-space: nowrap;
  color: var(--muted);
}
.tag--strong {
  color: var(--text);
  font-weight: 700;
}
.tag--success {
  color: var(--success);
  border-color: color-mix(in srgb, var(--success) 40%, transparent);
  background: color-mix(in srgb, var(--success) 12%, transparent);
}
.tag--warning {
  color: var(--warning);
  border-color: color-mix(in srgb, var(--warning) 40%, transparent);
  background: color-mix(in srgb, var(--warning) 12%, transparent);
}
.tag--danger {
  color: var(--danger);
  border-color: color-mix(in srgb, var(--danger) 50%, transparent);
}
</style>
