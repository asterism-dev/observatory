<script setup lang="ts">
import AuditTable from '@/components/AuditTable.vue'
import { useOrg } from '@/composables/useOrg'

const { org } = useOrg()
</script>

<template>
  <template v-if="org">
    <AuditTable :key="org.id" :org="org.id" />
  </template>
</template>
