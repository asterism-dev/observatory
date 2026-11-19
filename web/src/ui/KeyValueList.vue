<template>
  <dl class="kv"><slot /></dl>
</template>

<style scoped>
.kv {
  display: grid;
  grid-template-columns: max-content minmax(0, 1fr);
  align-items: center;
  gap: 0 24px;
  margin: 0;
}
.kv > :deep(dt),
.kv > :deep(dd) {
  padding: 9px 0;
}
.kv > :deep(dt:not(:first-of-type)),
.kv > :deep(dt:not(:first-of-type) + dd) {
  border-top: 1px solid var(--border);
}
.kv > :deep(dt) {
  color: var(--muted);
}
.kv > :deep(dd) {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px 8px;
  margin: 0;
  overflow-wrap: anywhere;
}
@media (max-width: 640px) {
  .kv {
    grid-template-columns: minmax(0, 1fr);
  }
  .kv > :deep(dt:not(:first-of-type) + dd) {
    border-top: 0;
    padding-top: 0;
  }
}
</style>
