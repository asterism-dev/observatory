<script setup lang="ts">
import type { Component } from 'vue'

import PageHeader from './PageHeader.vue'

defineProps<{ icon?: Component; title?: string; subtitle?: string; mono?: boolean }>()
</script>

<template>
  <div class="page">
    <PageHeader v-if="title" :icon="icon" :title="title" :subtitle="subtitle" :mono="mono">
      <template v-if="$slots.tags" #tags><slot name="tags" /></template>
      <slot name="actions" />
    </PageHeader>
    <div class="page-body"><slot /></div>
  </div>
</template>
