<script setup lang="ts">
import { Badge, Button, Dialog } from '@asterism-dev/lumen'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'

import { api, ApiError } from '@/api'
import { gb, powerActions, vmTones, type VmInfo } from '@/composables/nodes'
import { useOrg } from '@/composables/useOrg'

const { t, d } = useI18n()
const route = useRoute()
const router = useRouter()
const { org } = useOrg()

const vmId = computed(() => String(route.params.vmId))
const vm = ref<VmInfo | null>(null)
const error = ref('')
const busy = ref(false)
const confirmDelete = ref(false)

const base = () => `/api/orgs/${org.value?.id}/vms/${vmId.value}`
const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

async function load() {
  if (!org.value) return
  const id = org.value.id
  try {
    const res = await api<VmInfo>(base())
    if (id === org.value?.id) vm.value = res
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return router.replace(`/orgs/${id}/vms`)
    error.value = message(e)
  }
}

async function run(fn: () => Promise<void>) {
  busy.value = true
  error.value = ''
  try {
    await fn()
  } catch (e) {
    error.value = message(e)
    confirmDelete.value = false
  } finally {
    busy.value = false
  }
}

const power = (action: string) =>
  run(async () => {
    vm.value = await api<VmInfo>(`${base()}/${action}`, { method: 'POST' })
  })

const remove = () =>
  run(async () => {
    await api(base(), { method: 'DELETE' })
    router.replace(`/orgs/${org.value?.id}/vms`)
  })

const timer = window.setInterval(() => document.visibilityState === 'visible' && !busy.value && load(), 5000)
onBeforeUnmount(() => window.clearInterval(timer))

watch(
  () => [org.value?.id, vmId.value],
  () => {
    vm.value = null
    error.value = ''
    load()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <p><router-link :to="`/orgs/${org.id}/vms`">{{ t('nav.vms') }}</router-link></p>
    <p v-if="error" role="alert">{{ error }}</p>
    <template v-if="vm">
      <h1>{{ vm.name }} <Badge :tone="vmTones[vm.state]">{{ t(`vms.states.${vm.state}`) }}</Badge></h1>
      <p v-if="vm.error" role="alert">{{ vm.error }}</p>
      <dl>
        <dt>{{ t('vms.owner') }}</dt>
        <dd>{{ vm.owner.name }}</dd>
        <dt>{{ t('vms.node') }}</dt>
        <dd><router-link :to="`/orgs/${org.id}/nodes/${vm.node.id}`">{{ vm.node.name }}</router-link></dd>
        <dt>{{ t('vms.cpu') }}</dt>
        <dd>{{ vm.cpus }}</dd>
        <dt>{{ t('vms.memory') }}</dt>
        <dd>{{ gb(vm.memoryMB) }} GB</dd>
        <dt>{{ t('vms.disk') }}</dt>
        <dd>{{ vm.diskGB }} GB</dd>
        <dt>{{ t('vms.ip') }}</dt>
        <dd>{{ vm.ip || '–' }} <small v-if="vm.ip">({{ t('vms.ipHint') }})</small></dd>
        <dt>{{ t('vms.created') }}</dt>
        <dd>{{ d(new Date(vm.createdAt), { dateStyle: 'short', timeStyle: 'short' }) }}</dd>
      </dl>
      <p v-if="vm.canPower || vm.canDelete" class="row">
        <template v-if="vm.canPower">
          <Button v-for="a in powerActions(vm.state)" :key="a" :disabled="busy" @click="power(a)">{{ t(`vms.${a}`) }}</Button>
        </template>
        <Button v-if="vm.canDelete" variant="danger" :disabled="busy" @click="confirmDelete = true">{{ t('vms.delete') }}</Button>
      </p>
      <Dialog
        :open="confirmDelete"
        :title="t('vms.deleteTitle')"
        :description="t('vms.deleteBody', { name: vm.name })"
        @update:open="(o) => !o && !busy && (confirmDelete = false)"
      >
        <template #actions>
          <Button :disabled="busy" @click="confirmDelete = false">{{ t('profile.cancel') }}</Button>
          <Button variant="danger" :disabled="busy" @click="remove">{{ t('vms.delete') }}</Button>
        </template>
      </Dialog>
    </template>
  </template>
</template>

<style scoped>
.row {
  display: flex;
  gap: 8px;
}
dl {
  display: grid;
  grid-template-columns: max-content 1fr;
  gap: 4px 16px;
}
dd {
  margin: 0;
}
</style>
