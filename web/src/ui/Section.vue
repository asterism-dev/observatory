<script setup lang="ts">
defineProps<{ label?: string; description?: string }>()
</script>

<template>
  <section class="section">
    <div v-if="label || $slots.actions" class="section__head">
      <h2 v-if="label" class="section__label">{{ label }}</h2>
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
.section__label {
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
.section__label::before {
  content: '';
  width: 5px;
  height: 5px;
  background: currentColor;
  transform: rotate(45deg);
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
