<template>
  <div class="filter-bar">
    <slot />
    <span class="filter-bar__spacer" />
    <slot name="end" />
  </div>
</template>

<style scoped>
.filter-bar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 12px;
  margin-bottom: calc(16px - var(--section-gap));
}
.filter-bar__spacer {
  flex: 1;
}
</style>
