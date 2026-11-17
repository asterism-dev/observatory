<script setup lang="ts">
import { useI18n } from 'vue-i18n'

import AuditTable from '@/components/AuditTable.vue'
import { useOrg } from '@/composables/useOrg'

const { t } = useI18n()
const { org } = useOrg()
</script>

<template>
  <template v-if="org">
    <h1>{{ t('nav.orgAudit') }}</h1>
    <AuditTable :key="org.id" :source="`/api/orgs/${org.id}/audit`" />
  </template>
</template>
