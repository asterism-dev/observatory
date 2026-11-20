<script setup lang="ts">
import { Button, Dialog } from '@asterism-dev/lumen'
import { ArrowUpRight } from '@lucide/vue'
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import OpenVMDialog from '@/components/OpenVMDialog.vue'
import { gb, liveUsage, powerActions, relativeTime, vmTones, type VmInfo } from '@/composables/nodes'
import { useMe } from '@/composables/useMe'
import { Meter, ResourceList, ResourceRow, Tag } from '@/ui'

const props = defineProps<{ orgId: string; vms: VmInfo[]; empty: string }>()
const emit = defineEmits<{ changed: [] }>()
const { t, locale } = useI18n()
const { me } = useMe()

const error = ref('')
const busy = ref(false)
const pendingDelete = ref<VmInfo | null>(null)
const opening = ref<VmInfo | null>(null)

const isMine = (v: VmInfo) => v.owner.kind === 'user' && v.owner.id === me.value?.user.id

async function act(fn: () => Promise<unknown>) {
  busy.value = true
  error.value = ''
  try {
    await fn()
    emit('changed')
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    pendingDelete.value = null
    busy.value = false
  }
}

const power = (v: VmInfo, action: string) => act(() => api(`/api/orgs/${props.orgId}/vms/${v.id}/${action}`, { method: 'POST' }))
const remove = (v: VmInfo) => act(() => api(`/api/orgs/${props.orgId}/vms/${v.id}`, { method: 'DELETE' }))
</script>

<template>
  <p v-if="error" role="alert">{{ error }}</p>
  <ResourceList :count="vms.length" :empty="empty">
    <ResourceRow
      v-for="v in vms"
      :key="v.id"
      :title="v.name"
      :meta="`${v.cpus} vCPU · ${gb(v.memoryMB)} GB · ${v.diskGB} GB`"
      :to="`/orgs/${orgId}/vms/${v.id}`"
      :tone="vmTones[v.state]"
    >
      <template #tags>
        <Tag strong>{{ v.node.name }}</Tag>
        <Tag>{{ isMine(v) ? t('vms.mine') : v.owner.name }}</Tag>
        <Tag :tone="vmTones[v.state]">{{ t(`vms.states.${v.state}`) }}</Tag>
        <Tag v-if="v.deleteAt" tone="warning">{{ t('vms.deletesIn', { rel: relativeTime(v.deleteAt, locale) }) }}</Tag>
        <Tag v-if="v.legacyRuntime" tone="warning">{{ t('vms.legacyRuntime') }}</Tag>
      </template>
      <Meter :label="t('usage.cpu')" :percent="liveUsage(v).cpu" />
      <Meter :label="t('usage.mem')" :percent="liveUsage(v).mem" />
      <Meter :label="t('usage.dsk')" :percent="liveUsage(v).dsk" />
      <template #actions>
        <template v-if="v.canPower">
          <Button v-for="a in powerActions(v.state)" :key="a" size="sm" variant="ghost" :disabled="busy" @click="power(v, a)">{{ t(`vms.${a}`) }}</Button>
        </template>
        <Button v-if="v.canDelete" size="sm" variant="ghost" :disabled="busy" @click="pendingDelete = v">{{ t('vms.delete') }}</Button>
        <Button size="sm" :icon="ArrowUpRight" @click="opening = v">{{ t('nodes.open') }}</Button>
      </template>
    </ResourceRow>
  </ResourceList>
  <OpenVMDialog :open="!!opening" :org-id="orgId" :vm="opening" @update:open="(o) => !o && (opening = null)" />
  <Dialog
    :open="!!pendingDelete"
    :title="t('vms.deleteTitle')"
    :description="t('vms.deleteBody', { name: pendingDelete?.name })"
    @update:open="(o) => !o && !busy && (pendingDelete = null)"
  >
    <template #actions>
      <Button :disabled="busy" @click="pendingDelete = null">{{ t('profile.cancel') }}</Button>
      <Button variant="danger" :disabled="busy" @click="pendingDelete && remove(pendingDelete)">{{ t('vms.delete') }}</Button>
    </template>
  </Dialog>
</template>
