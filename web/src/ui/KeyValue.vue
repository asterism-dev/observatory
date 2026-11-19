<script setup lang="ts">
defineProps<{ label: string }>()
</script>

<template>
  <dt>{{ label }}</dt>
  <dd><slot /></dd>
</template>
