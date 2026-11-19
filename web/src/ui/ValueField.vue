<script setup lang="ts">
import CopyButton from './CopyButton.vue'

defineProps<{ label: string; value?: string; mono?: boolean; copy?: boolean }>()
</script>

<template>
  <div class="value-field">
    <div class="value-field__head">
      <span class="value-field__label">{{ label }}</span>
      <slot name="actions" />
    </div>
    <div class="value-field__value" :class="{ mono }">
      <slot>{{ value || '–' }}</slot>
      <CopyButton v-if="copy && value" :text="value" />
    </div>
  </div>
</template>

<style scoped>
.value-field {
  display: grid;
  gap: 6px;
}
.value-field__head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 8px;
}
.value-field__label {
  font-size: 0.75rem;
  font-weight: 600;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--muted);
}
.value-field__value {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px;
  font-size: 1.05rem;
  overflow-wrap: anywhere;
}
</style>
