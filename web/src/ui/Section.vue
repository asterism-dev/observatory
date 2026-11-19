<script setup lang="ts">
import Eyebrow from './Eyebrow.vue'

defineProps<{ label?: string; description?: string }>()
</script>

<template>
  <section class="section">
    <div v-if="label || $slots.actions" class="section__head">
      <Eyebrow v-if="label" as="h2">{{ label }}</Eyebrow>
      <div class="section__actions"><slot name="actions" /></div>
    </div>
    <p v-if="description" class="section__description">{{ description }}</p>
    <slot />
  </section>
</template>

<style scoped>
.section {
  display: grid;
  gap: 10px;
  min-width: 0;
}
.section__head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 8px;
  min-height: 24px;
}
.section__actions {
  display: flex;
  gap: 8px;
}
.section__description {
  margin: -4px 0 0;
  color: var(--muted);
}
</style>
