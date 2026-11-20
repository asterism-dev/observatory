<script setup lang="ts">
import { Button, Dialog, NumberInput, Select, Tabs, TextInput } from '@asterism-dev/lumen'
import { ArrowUpRight, Box } from '@lucide/vue'
import { computed, onBeforeUnmount, ref, watch, watchEffect } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'

import { api, ApiError } from '@/api'
import OpenVMDialog from '@/components/OpenVMDialog.vue'
import VMDisksDialog from '@/components/VMDisksDialog.vue'
import VMDisksTab from '@/components/VMDisksTab.vue'
import VMNetworkingTab from '@/components/VMNetworkingTab.vue'
import VMNetworksDialog from '@/components/VMNetworksDialog.vue'
import VMSnapshots from '@/components/VMSnapshots.vue'
import VMUsage from '@/components/VMUsage.vue'
import { defaultSourceLabel } from '@/composables/images'
import { kindLabels, type IntegrationKind } from '@/composables/integrations'
import { gb, powerActions, relativeTime, vmTones, type VmInfo } from '@/composables/nodes'
import { useCrumbs } from '@/composables/useCrumbs'
import { useOrg } from '@/composables/useOrg'
import { Card, CommandBox, KeyValue, KeyValueList, PageHeader, Section, Tag } from '@/ui'

const { t, d, locale } = useI18n()
const route = useRoute()
const router = useRouter()
const { org } = useOrg()
const { setCrumbs } = useCrumbs()

const vmId = computed(() => String(route.params.vmId))
const tabIds = ['overview', 'networking', 'snapshots', 'disks', 'settings']
const tabs = computed(() => tabIds.map((v) => ({ value: v, label: t(`vms.tabs.${v}`) })))
const tab = computed({
  get: () => (tabIds.includes(String(route.query.tab)) ? String(route.query.tab) : 'overview'),
  set: (v) => router.replace({ query: { ...route.query, tab: v === 'overview' ? undefined : v } }),
})
const vm = ref<VmInfo | null>(null)
const error = ref('')
const busy = ref(false)
const confirmDelete = ref(false)
const networksOpen = ref(false)
const disksOpen = ref(false)
const openOpen = ref(false)
const egress = ref<{ secrets: number; hosts: string[]; caNotAfter: string | null; trustChanged: boolean } | null>(null)
const integrations = ref<{ kind: IntegrationKind; scope?: string; teamName?: string; mode?: string }[]>([])

watchEffect(() => {
  if (vm.value && org.value) setCrumbs(route.path, [{ label: vm.value.name, to: route.path }, { label: t(`vms.tabs.${tab.value}`) }])
})

const base = () => `/api/orgs/${org.value?.id}/vms/${vmId.value}`
const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

async function load() {
  if (!org.value) return
  const id = org.value.id
  try {
    const [res, eg, ig] = await Promise.all([
      api<VmInfo>(base()),
      api<NonNullable<typeof egress.value>>(`${base()}/egress`).catch(() => null),
      api<{ integrations: typeof integrations.value }>(`${base()}/integrations`).then((r) => r.integrations, () => []),
    ])
    if (id !== org.value?.id) return
    vm.value = res
    egress.value = eg
    integrations.value = ig
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return router.replace(`/orgs/${id}/vms`)
    error.value = message(e)
  }
}

function integrationLine(i: (typeof integrations.value)[number]) {
  const scope = i.scope === 'team' ? `${t('env.scopeTeam')} ${i.teamName ?? ''}`.trim() : i.scope && t(i.scope === 'org' ? 'env.scopeOrg' : 'env.scopePrivate')
  return [kindLabels[i.kind], scope, i.mode].filter(Boolean).join(' · ')
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

const presets = [0, 15, 60, 240]
const suspendOptions = computed(() => [
  ...presets.map((m) => ({ value: String(m), label: m ? t('vms.autoSuspendMinutes', { n: m }) : t('vms.autoSuspendOff') })),
  { value: 'custom', label: t('vms.autoSuspendCustom') },
])
const suspendChoice = ref('0')
const customMinutes = ref(30)
const customValid = computed(() => Number.isInteger(customMinutes.value) && customMinutes.value >= 5 && customMinutes.value <= 1440)

const setAutoSuspend = (minutes: number) =>
  run(async () => {
    vm.value = await api<VmInfo>(`${base()}/auto-suspend`, { method: 'PUT', body: JSON.stringify({ minutes }) })
  })

watch(
  () => vm.value?.autoSuspendMinutes,
  (m) => {
    if (m === undefined || busy.value) return
    suspendChoice.value = presets.includes(m) ? String(m) : 'custom'
    if (!presets.includes(m)) customMinutes.value = m
  },
  { immediate: true },
)

const deleteAfter = ref('')
const setDeleteAfter = (value: string | null) =>
  run(async () => {
    vm.value = await api<VmInfo>(`${base()}/delete-after`, { method: 'PUT', body: JSON.stringify({ deleteAfter: value }) })
    deleteAfter.value = ''
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
    egress.value = null
    integrations.value = []
    error.value = ''
    load()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <p v-if="error && !vm" role="alert" class="load-error">{{ error }}</p>
    <template v-if="vm">
      <PageHeader
        :icon="Box"
        :title="vm.name"
        mono
        :subtitle="`${vm.owner.name} · ${vm.cpus} vCPU · ${gb(vm.memoryMB)} GB · ${vm.diskGB} GB`"
      >
        <template #tags>
          <Tag :tone="vmTones[vm.state]">{{ t(`vms.states.${vm.state}`) }}</Tag>
          <Tag strong>{{ vm.node.name }}</Tag>
          <Tag v-if="vm.legacyRuntime" tone="warning">{{ t('vms.legacyRuntime') }}</Tag>
        </template>
        <template #actions>
          <Button variant="accent" :icon="ArrowUpRight" @click="openOpen = true">{{ t('nodes.open') }}</Button>
          <template v-if="vm.canPower">
            <Button v-for="a in powerActions(vm.state)" :key="a" :disabled="busy" @click="power(a)">{{ t(`vms.${a}`) }}</Button>
          </template>
        </template>
      </PageHeader>
      <div class="page-body">
      <p v-if="error" role="alert">{{ error }}</p>
      <p v-if="vm.error" role="alert">{{ vm.error }}</p>
      <Tabs v-model="tab" :tabs="tabs" variant="underline" />

      <template v-if="tab === 'overview'">
        <VMUsage :org-id="org.id" :vm="vm" />
        <Section :label="t('vms.connect')">
          <Card>
            <template v-if="vm.dnsName">
              <CommandBox :command="`ssh dev@${vm.dnsName}`" />
              <template v-if="vm.image">
                <CommandBox :command="`xpra attach ssh://dev@${vm.dnsName}/100`" />
                <small class="muted">
                  {{ t('vms.desktop') }} ·
                  <a href="https://github.com/Xpra-org/xpra/wiki/Download" target="_blank" rel="noopener">{{ t('vms.desktopInstall') }}</a>
                </small>
              </template>
            </template>
            <span v-else class="muted">–</span>
          </Card>
        </Section>
        <div class="split">
          <Section :label="t('vms.details')">
            <Card>
              <KeyValueList>
                <KeyValue :label="t('vms.owner')">{{ vm.owner.name }}</KeyValue>
                <KeyValue :label="t('vms.node')"><router-link :to="`/orgs/${org.id}/nodes/${vm.node.id}`">{{ vm.node.name }}</router-link></KeyValue>
                <KeyValue :label="t('vms.image')">
                  <code>{{ vm.imageRef || vm.image || '–' }}</code>
                  <span v-if="vm.imageSource" class="muted">{{ defaultSourceLabel(vm.imageSource, t) }}</span>
                </KeyValue>
                <KeyValue v-if="integrations.length" :label="t('integrations.vmLine')">{{ integrations.map(integrationLine).join(', ') }}</KeyValue>
                <KeyValue :label="t('vms.created')">{{ d(new Date(vm.createdAt), { dateStyle: 'short', timeStyle: 'short' }) }}</KeyValue>
              </KeyValueList>
            </Card>
          </Section>
          <Section :label="t('networks.section')">
            <Card>
              <span class="tags">
                <Tag v-for="n in vm.networks" :key="n.id">{{ n.name }}</Tag>
                <Tag v-if="!vm.networks.length">{{ t('networks.none') }}</Tag>
                <Tag v-if="egress">{{ t('vms.egressSecrets', { n: egress.secrets }) }}</Tag>
              </span>
            </Card>
          </Section>
        </div>
      </template>

      <VMNetworkingTab v-if="tab === 'networking'" :vm="vm!" :egress="egress" :busy="busy" @edit="networksOpen = true" />
      <VMDisksTab v-if="tab === 'disks'" :vm="vm!" :busy="busy" @edit="disksOpen = true" />

      <template v-if="tab === 'snapshots'">
        <VMSnapshots :org-id="org.id" :vm="vm!" @changed="load" />
      </template>

      <template v-if="tab === 'settings'">
        <Section v-if="vm!.canPower" :label="t('vms.autoSuspend')" :description="t('vms.autoSuspendHint')">
          <Card>
            <p class="row">
              <Select
                v-model="suspendChoice"
                :options="suspendOptions"
                :disabled="busy"
                :aria-label="t('vms.autoSuspend')"
                @update:model-value="(c) => c !== 'custom' && setAutoSuspend(Number(c))"
              />
              <template v-if="suspendChoice === 'custom'">
                <NumberInput v-model="customMinutes" :aria-label="t('vms.autoSuspendCustom')" :invalid="!customValid" />
                <Button :disabled="busy || !customValid" @click="setAutoSuspend(customMinutes)">{{ t('vms.autoSuspendApply') }}</Button>
              </template>
            </p>
          </Card>
        </Section>
        <Section v-if="vm!.canDelete" :label="t('vms.deleteAfter')" :description="t('vms.deleteAfterHint')">
          <Card>
            <span class="muted">{{
              !vm!.deleteAt
                ? t('vms.deleteAfterNever')
                : Date.parse(vm!.deleteAt) <= Date.now()
                  ? t('vms.deleting')
                  : t('vms.deletesAt', { at: new Date(vm!.deleteAt).toLocaleString(locale), rel: relativeTime(vm!.deleteAt, locale) })
            }}</span>
            <p class="row">
              <TextInput v-model="deleteAfter" :aria-label="t('vms.deleteAfter')" placeholder="2d" />
              <Button :disabled="busy || !deleteAfter.trim()" @click="setDeleteAfter(deleteAfter.trim())">{{ t('vms.autoSuspendApply') }}</Button>
              <Button v-if="vm!.deleteAt" :disabled="busy" @click="setDeleteAfter(null)">{{ t('vms.deleteAfterClear') }}</Button>
            </p>
          </Card>
        </Section>
        <Section v-if="vm!.canDelete" :label="t('vms.deleteTitle')">
          <Card tone="danger">
            <span class="muted">{{ t('vms.deleteBody', { name: vm!.name }) }}</span>
            <p><Button variant="danger" :disabled="busy" @click="confirmDelete = true">{{ t('vms.delete') }}</Button></p>
          </Card>
        </Section>
      </template>


      </div>
    </template>
    <template v-if="vm && org">
    <OpenVMDialog v-model:open="openOpen" :org-id="org.id" :vm="vm" />
    <VMNetworksDialog v-model:open="networksOpen" :org-id="org.id" :vm="vm" @saved="load" />
    <VMDisksDialog v-model:open="disksOpen" :org-id="org.id" :vm="vm" @saved="load" />
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
.load-error {
  margin: var(--page-y) var(--page-x);
}
.split {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(300px, 1fr));
  align-items: start;
  gap: var(--section-gap) 16px;
}
.tags,
.row {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 8px;
  margin: 0;
}
</style>
