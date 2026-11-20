<script setup lang="ts">
import type { TableColumn } from '@asterism-dev/lumen'
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

import { gb, lifetime, type VmHistory } from '@/composables/nodes'
import { useMe } from '@/composables/useMe'
import { DataTable } from '@/ui'

defineProps<{ vms: VmHistory[] }>()
const { t, d } = useI18n()
const { me } = useMe()

const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('vms.name') },
  { key: 'owner', label: t('vms.owner') },
  { key: 'size', label: t('vms.history.size') },
  { key: 'createdAt', label: t('vms.history.created') },
  { key: 'deletedAt', label: t('vms.history.deleted') },
  { key: 'lifetime', label: t('vms.history.lifetime') },
])
const when = (iso: string) => d(new Date(iso), { dateStyle: 'short', timeStyle: 'short' })
const who = (u: VmHistory['createdBy']) => u?.name || t('vms.history.unknown')
const owner = (v: VmHistory) => (v.owner.kind === 'user' && v.owner.id === me.value?.user.id ? t('vms.mine') : v.owner.name || t('vms.history.unknown'))
</script>

<template>
  <DataTable :columns="columns" :rows="vms as unknown as Record<string, unknown>[]" row-key="id">
    <template #cell-owner="{ row }">{{ owner(row as VmHistory) }}</template>
    <template #cell-size="{ row }">{{ row.cpus }} vCPU · {{ gb(row.memoryMB as number) }} GB · {{ row.diskGB }} GB</template>
    <template #cell-createdAt="{ row }">{{ when(row.createdAt as string) }} · {{ who((row as VmHistory).createdBy) }}</template>
    <template #cell-deletedAt="{ row }">
      {{ when(row.deletedAt as string) }} ·
      {{ row.reason === 'expired' ? t('vms.history.expired') : who((row as VmHistory).deletedBy) }}
    </template>
    <template #cell-lifetime="{ row }">{{ lifetime(row.createdAt as string, row.deletedAt as string) }}</template>
    <template #empty>{{ t('vms.history.empty') }}</template>
  </DataTable>
</template>
