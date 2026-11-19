<script setup lang="ts">
import { Badge, Button, Dialog, NumberInput, Select, Tabs } from '@asterism-dev/lumen'
import { ArrowUpRight } from '@lucide/vue'
import { computed, onBeforeUnmount, ref, watch, watchEffect } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'

import { api, ApiError } from '@/api'
import OpenVMDialog from '@/components/OpenVMDialog.vue'
import VMDisksDialog from '@/components/VMDisksDialog.vue'
import VMNetworksDialog from '@/components/VMNetworksDialog.vue'
import VMSnapshots from '@/components/VMSnapshots.vue'
import VMUsage from '@/components/VMUsage.vue'
import { kindLabels, type IntegrationKind } from '@/composables/integrations'
import { gb, powerActions, vmTones, type VmInfo } from '@/composables/nodes'
import { useCrumbs } from '@/composables/useCrumbs'
import { useOrg } from '@/composables/useOrg'

const { t, d } = useI18n()
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
const copyFailed = ref(false)
const desktopCopyFailed = ref(false)
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

const remove = () =>
  run(async () => {
    await api(base(), { method: 'DELETE' })
    router.replace(`/orgs/${org.value?.id}/vms`)
  })

async function copyDesktop() {
  desktopCopyFailed.value = false
  try {
    await navigator.clipboard.writeText(`xpra attach ssh://ubuntu@${vm.value?.dnsName}/100`)
  } catch {
    desktopCopyFailed.value = true
  }
}

async function copySsh() {
  copyFailed.value = false
  try {
    await navigator.clipboard.writeText(`ssh ubuntu@${vm.value?.dnsName}`)
  } catch {
    copyFailed.value = true
  }
}

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
    <p v-if="error" role="alert">{{ error }}</p>
    <template v-if="vm">
      <header class="head">
        <div class="title">
          <h1>{{ vm.name }}</h1>
          <Badge :tone="vmTones[vm.state]">{{ t(`vms.states.${vm.state}`) }}</Badge>
          <Badge variant="outline" weight="bold">{{ vm.node.name }}</Badge>
        </div>
        <p class="row">
          <Button variant="accent" :icon="ArrowUpRight" @click="openOpen = true">{{ t('nodes.open') }}</Button>
          <template v-if="vm.canPower">
            <Button v-for="a in powerActions(vm.state)" :key="a" :disabled="busy" @click="power(a)">{{ t(`vms.${a}`) }}</Button>
          </template>
        </p>
        <p class="sub">
          {{ vm.owner.name }} · {{ vm.cpus }} vCPU · {{ gb(vm.memoryMB) }} GB · {{ vm.diskGB }} GB
        </p>
      </header>
      <p v-if="vm.error" role="alert">{{ vm.error }}</p>
      <Tabs v-model="tab" :tabs="tabs" variant="underline" />

      <div v-if="tab === 'overview'" class="panel">
        <VMUsage :org-id="org.id" :vm="vm" />
        <section>
          <h2 class="label">{{ t('vms.connect') }}</h2>
          <dl class="card kv">
            <dt>{{ t('vms.ssh') }}</dt>
            <dd>
              <template v-if="vm.dnsName">
                <code>ssh ubuntu@{{ vm.dnsName }}</code>
                <Button size="sm" @click="copySsh">{{ t('networks.copy') }}</Button>
                <small v-if="copyFailed" role="alert">{{ t('networks.copyFailed') }}</small>
              </template>
              <template v-else>–</template>
            </dd>
            <template v-if="vm.image && vm.dnsName">
              <dt>{{ t('vms.desktop') }}</dt>
              <dd>
                <code>xpra attach ssh://ubuntu@{{ vm.dnsName }}/100</code>
                <Button size="sm" @click="copyDesktop">{{ t('networks.copy') }}</Button>
                <small v-if="desktopCopyFailed" role="alert">{{ t('networks.copyFailed') }}</small>
                <a href="https://github.com/Xpra-org/xpra/wiki/Download" target="_blank" rel="noopener">{{ t('vms.desktopInstall') }}</a>
              </dd>
            </template>
          </dl>
        </section>
        <div class="split">
          <section>
            <h2 class="label">{{ t('vms.details') }}</h2>
            <dl class="card kv">
              <dt>{{ t('vms.owner') }}</dt>
              <dd>{{ vm.owner.name }}</dd>
              <dt>{{ t('vms.node') }}</dt>
              <dd><router-link :to="`/orgs/${org.id}/nodes/${vm.node.id}`">{{ vm.node.name }}</router-link></dd>
              <dt>{{ t('vms.image') }}</dt>
              <dd>{{ vm.image || (vm.stockImage ? t('vms.imageStock') : '–') }}</dd>
              <template v-if="integrations.length">
                <dt>{{ t('integrations.vmLine') }}</dt>
                <dd>{{ integrations.map(integrationLine).join(', ') }}</dd>
              </template>
              <dt>{{ t('vms.created') }}</dt>
              <dd>{{ d(new Date(vm.createdAt), { dateStyle: 'short', timeStyle: 'short' }) }}</dd>
            </dl>
          </section>
          <section>
            <h2 class="label">{{ t('networks.section') }}</h2>
            <p class="card badges">
              <Badge v-for="n in vm.networks" :key="n.id" variant="outline">{{ n.name }}</Badge>
              <Badge v-if="!vm.networks.length" variant="outline">{{ t('networks.none') }}</Badge>
              <Badge v-if="egress" variant="outline">{{ t('vms.egressSecrets', { n: egress.secrets }) }}</Badge>
            </p>
          </section>
        </div>
      </div>

      <div v-else-if="tab === 'networking'" class="panel">
        <div class="split">
          <section>
            <h2 class="label">{{ t('vms.connect') }}</h2>
            <dl class="card kv">
              <dt>{{ t('vms.ip') }}</dt>
              <dd>{{ vm.ip || '–' }} <small v-if="vm.ip">({{ t('vms.ipHint') }})</small></dd>
              <dt>{{ t('networks.mgmt') }}</dt>
              <dd>{{ vm.mgmtIP || '–' }}</dd>
              <dt>{{ t('networks.dns') }}</dt>
              <dd>{{ vm.dnsName || '–' }}</dd>
            </dl>
          </section>
          <section v-if="egress">
            <h2 class="label">{{ t('vms.egress') }}</h2>
            <dl class="card kv">
              <dt>{{ t('vms.egress') }}</dt>
              <dd>{{ t('vms.egressSecrets', { n: egress.secrets }) }}</dd>
              <template v-if="egress.hosts.length">
                <dt>{{ t('vms.egressHosts') }}</dt>
                <dd><Badge v-for="h in egress.hosts" :key="h">{{ h }}</Badge></dd>
              </template>
              <template v-if="egress.caNotAfter">
                <dt>{{ t('vms.egressCaLabel') }}</dt>
                <dd>{{ t('vms.egressCa', { date: d(new Date(egress.caNotAfter), { dateStyle: 'short' }) }) }}</dd>
              </template>
            </dl>
            <small v-if="egress.trustChanged" role="status">{{ t('vms.egressRestart') }}</small>
          </section>
        </div>
        <section>
          <div class="section-head">
            <h2 class="label">{{ t('networks.section') }}</h2>
            <Button v-if="vm.canNetwork" size="sm" :disabled="busy" @click="networksOpen = true">{{ t('networks.change') }}</Button>
          </div>
          <dl v-if="vm.networks.length" class="card kv">
            <template v-for="n in vm.networks" :key="n.id">
              <dt>{{ n.name }}</dt>
              <dd><code>{{ n.ip }}</code> <span class="muted">{{ n.dnsName }}</span></dd>
            </template>
          </dl>
          <p v-else class="card muted">{{ t('networks.none') }}</p>
        </section>
      </div>

      <div v-else-if="tab === 'snapshots'" class="panel">
        <div class="card"><VMSnapshots :org-id="org.id" :vm="vm" @changed="load" /></div>
      </div>

      <div v-else-if="tab === 'disks'" class="panel">
        <section>
          <div class="section-head">
            <h2 class="label">{{ t('vmDisks.section') }}</h2>
            <Button v-if="vm.canDisks" size="sm" :disabled="busy" @click="disksOpen = true">{{ t('vmDisks.change') }}</Button>
          </div>
          <dl class="card kv">
            <dt>{{ t('vmDisks.root') }}</dt>
            <dd><code>/</code> <span class="muted">{{ vm.diskGB }} GB</span></dd>
            <template v-for="dk in vm.disks" :key="dk.id">
              <dt>{{ dk.name }}</dt>
              <dd>
                <code>{{ dk.mountPath }}</code> <span class="muted">{{ dk.sizeGB }} GB</span>
                <Badge :tone="dk.error ? 'danger' : 'muted'">{{ t(`disks.states.${dk.state}`) }}</Badge>
                <small v-if="dk.error" role="alert">{{ dk.error }}</small>
              </dd>
            </template>
          </dl>
        </section>
      </div>

      <div v-else class="panel">
        <section v-if="vm.canPower">
          <h2 class="label">{{ t('vms.autoSuspend') }}</h2>
          <div class="card stack">
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
            <small class="muted">{{ t('vms.autoSuspendHint') }}</small>
          </div>
        </section>
        <section v-if="vm.canDelete">
          <h2 class="label">{{ t('vms.deleteTitle') }}</h2>
          <div class="card stack danger-zone">
            <span class="muted">{{ t('vms.deleteBody', { name: vm.name }) }}</span>
            <p><Button variant="danger" :disabled="busy" @click="confirmDelete = true">{{ t('vms.delete') }}</Button></p>
          </div>
        </section>
      </div>

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
.head {
  display: grid;
  grid-template-columns: 1fr auto;
  align-items: center;
  gap: 4px 16px;
  margin-bottom: 16px;
}
.title {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px;
}
h1 {
  margin: 0;
  font-family: var(--font-mono, ui-monospace, monospace);
  font-size: 1.5rem;
}
.sub {
  grid-column: 1 / -1;
  margin: 0;
  color: var(--muted);
}
.muted {
  color: var(--muted);
}
.section-head {
  display: flex;
  justify-content: space-between;
  align-items: baseline;
  gap: 8px;
}
.stack {
  display: grid;
  gap: 10px;
}
.stack p {
  margin: 0;
}
.danger-zone {
  border-color: var(--danger);
}
.panel {
  display: grid;
  gap: 28px;
  padding-top: 20px;
}
.label {
  margin: 0 0 10px;
  font-family: var(--font-mono, ui-monospace, monospace);
  font-size: 0.7rem;
  font-weight: normal;
  letter-spacing: 0.08em;
  text-transform: uppercase;
  color: var(--muted);
}
.row {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin: 0;
}
.badges {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin: 0;
}
.card {
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--panel);
  padding: 12px 16px;
}
.split {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
  align-items: start;
  gap: 28px 16px;
}
.kv {
  align-items: center;
}
.kv > dt {
  color: var(--muted);
}
.kv > dt:not(:first-of-type),
.kv > dt:not(:first-of-type) + dd {
  padding-top: 8px;
  border-top: 1px solid var(--border);
}
.kv > dd {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px 8px;
  min-width: 0;
  overflow-wrap: anywhere;
}
dl {
  display: grid;
  grid-template-columns: max-content 1fr;
  gap: 8px 16px;
  margin: 0;
}
dd {
  margin: 0;
}
@media (max-width: 640px) {
  .head {
    grid-template-columns: 1fr;
  }
}
</style>
