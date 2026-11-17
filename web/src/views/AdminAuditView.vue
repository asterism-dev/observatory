<script setup lang="ts">
import { useI18n } from 'vue-i18n'

import AuditTable from '@/components/AuditTable.vue'

const { t } = useI18n()
</script>

<template>
  <h1>{{ t('nav.audit') }}</h1>
  <AuditTable source="/api/admin/audit" />
</template>
