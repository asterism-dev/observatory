<script setup lang="ts">
import { Badge, Button, Dialog, Tabs, TextInput } from '@asterism-dev/lumen'
import { computed, ref, watch, watchEffect } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'

import { api, ApiError } from '@/api'
import InstallCommand from '@/components/InstallCommand.vue'
import Meter from '@/components/Meter.vue'
import VMList from '@/components/VMList.vue'
import { gb, imageLine, stateTones, type NodeInfo, type VmInfo } from '@/composables/nodes'
import { useCrumbs } from '@/composables/useCrumbs'
import { useOrg } from '@/composables/useOrg'

const { t, d, locale } = useI18n()
const route = useRoute()
const router = useRouter()
const { org } = useOrg()

const nodeId = computed(() => String(route.params.nodeId))
const tabIds = ['overview', 'vms', 'settings']
const tabs = computed(() => tabIds.map((v) => ({ value: v, label: t(`nodes.tabs.${v}`) })))
const tab = computed({
  get: () => (tabIds.includes(String(route.query.tab)) ? String(route.query.tab) : 'overview'),
  set: (v) => router.replace({ query: { ...route.query, tab: v === 'overview' ? undefined : v } }),
})
const pct = (used: number, total: number) => (total ? (used / total) * 100 : null)
const node = ref<NodeInfo | null>(null)
const { setCrumbs } = useCrumbs()
watchEffect(() => {
  if (node.value) setCrumbs(route.path, [{ label: node.value.name, to: route.path }, { label: t(`nodes.tabs.${tab.value}`) }])
})
const vms = ref<VmInfo[]>([])
const name = ref('')
const error = ref('')
const busy = ref(false)
const confirmRegen = ref(false)
const command = ref('')
const confirmDelete = ref(false)

const machine = computed(() => Object.entries(node.value?.machine ?? {}))
const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

async function load() {
  if (!org.value) return
  const id = org.value.id
  try {
    const n = await api<NodeInfo>(`/api/orgs/${id}/nodes/${nodeId.value}`)
    if (id !== org.value?.id) return
    node.value = n
    name.value = n.name
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return router.replace(`/orgs/${id}/nodes`)
    error.value = message(e)
    return
  }
  try {
    const res = await api<{ vms: VmInfo[] }>(`/api/orgs/${id}/vms`)
    vms.value = res.vms.filter((v) => v.node.id === nodeId.value)
  } catch {
    // Listing VMs needs its own permission; the section just stays empty.
    vms.value = []
  }
}

async function run(fn: () => Promise<void>) {
  busy.value = true
  error.value = ''
  try {
    await fn()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = false
  }
}

const base = () => `/api/orgs/${org.value?.id}/nodes/${nodeId.value}`

const rename = () =>
  run(async () => {
    await api(base(), { method: 'PATCH', body: JSON.stringify({ name: name.value.trim() }) })
    await load()
  })

const regenerate = () =>
  run(async () => {
    const res = await api<{ installCommand: string }>(`${base()}/credential`, { method: 'POST' })
    confirmRegen.value = false
    command.value = res.installCommand
    await load()
  })

const remove = () =>
  run(async () => {
    try {
      await api(base(), { method: 'DELETE' })
    } catch (e) {
      confirmDelete.value = false
      throw e
    }
    router.replace(`/orgs/${org.value?.id}/nodes`)
  })

watch(
  () => [org.value?.id, nodeId.value],
  () => {
    node.value = null
    vms.value = []
    error.value = ''
    load()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <p v-if="error" role="alert">{{ error }}</p>
    <template v-if="node">
      <header class="head">
        <div class="title">
          <h1>{{ node.name }}</h1>
          <Badge :tone="stateTones[node.state]">{{ t(`nodes.states.${node.state}`) }}</Badge>
          <Badge v-if="node.quarantined" tone="danger">{{ t('nodes.quarantined') }}</Badge>
        </div>
        <p class="sub">
          {{ node.cpus }} vCPU · {{ gb(node.memoryMB) }} GB · {{ node.version || '–' }}
        </p>
      </header>
      <p v-if="node.quarantined" role="alert">{{ t('nodes.quarantinedHelp') }}</p>
      <Tabs v-model="tab" :tabs="tabs" variant="underline" />

      <div v-if="tab === 'overview'" class="panel">
        <section v-if="node.state !== 'pending'">
          <h2 class="label">{{ t('nodes.allocated') }}</h2>
          <div class="cards">
            <div class="card">
              <div class="mono muted">{{ t('nodes.cpu') }}</div>
              <strong>{{ node.usedCPUs }} / {{ node.cpus }}</strong>
              <Meter :label="t('usage.cpu')" :percent="pct(node.usedCPUs, node.cpus)" />
            </div>
            <div class="card">
              <div class="mono muted">{{ t('nodes.memory') }}</div>
              <strong>{{ gb(node.usedMemoryMB) }} / {{ gb(node.memoryMB) }} GB</strong>
              <Meter :label="t('usage.mem')" :percent="pct(node.usedMemoryMB, node.memoryMB)" />
            </div>
            <div class="card">
              <div class="mono muted">{{ t('nodes.vms') }}</div>
              <strong>{{ node.vmCount }}</strong>
            </div>
          </div>
        </section>
        <section>
          <h2 class="label">{{ t('nodes.machine') }}</h2>
          <dl class="card kv">
            <dt>{{ t('nodes.version') }}</dt>
            <dd>{{ node.version || '–' }}</dd>
            <dt>{{ t('nodes.guestImage') }}</dt>
            <dd><i18n-t v-if="node.image?.state === 'custom'" keypath="nodes.image.custom" tag="span"><template #key><code>imageURL</code></template></i18n-t>
            <template v-else>{{ imageLine(node.image, t, locale) }}</template></dd>
            <dt>{{ t('nodes.lastSeen') }}</dt>
            <dd>{{ node.lastSeen ? d(new Date(node.lastSeen), { dateStyle: 'short', timeStyle: 'short' }) : '–' }}</dd>
            <template v-if="node.underlay">
              <dt>{{ t('nodes.underlay') }}</dt>
              <dd>{{ node.underlay.ip }}</dd>
              <dt>{{ t('nodes.registered') }}</dt>
              <dd>{{ node.underlay.registered ? t('nodes.yes') : t('nodes.no') }}</dd>
              <dt>{{ t('nodes.handshake') }}</dt>
              <dd>{{ node.underlay.lastHandshake ? d(new Date(node.underlay.lastHandshake), { dateStyle: 'short', timeStyle: 'short' }) : t('nodes.never') }}</dd>
            </template>
            <template v-for="[k, v] in machine" :key="k">
              <dt>{{ k }}</dt>
              <dd>{{ v }}</dd>
            </template>
          </dl>
        </section>
      </div>

      <div v-else-if="tab === 'vms'" class="panel">
        <VMList :org-id="org.id" :vms="vms" :empty="t('nodes.noVms')" @changed="load" />
      </div>

      <div v-else class="panel">
        <section v-if="node.canUpdate">
          <h2 class="label">{{ t('nodes.rename') }}</h2>
          <form class="card row" @submit.prevent="rename">
            <TextInput v-model="name" :aria-label="t('nodes.name')" />
            <Button type="submit" :disabled="busy || !name.trim() || name.trim() === node.name">{{ t('nodes.rename') }}</Button>
          </form>
        </section>
        <section v-if="node.canUpdate">
          <h2 class="label">{{ t('nodes.regenerateTitle') }}</h2>
          <div class="card stack">
            <span class="muted">{{ t('nodes.regenerateBody') }}</span>
            <p><Button :disabled="busy" @click="confirmRegen = true">{{ t('nodes.regenerate') }}</Button></p>
          </div>
        </section>
        <section v-if="node.canDelete">
          <h2 class="label">{{ t('nodes.deleteTitle') }}</h2>
          <div class="card stack danger-zone">
            <span class="muted">{{ t('nodes.deleteBody', { name: node.name }) }}</span>
            <p><Button variant="danger" :disabled="busy" @click="confirmDelete = true">{{ t('nodes.delete') }}</Button></p>
          </div>
        </section>
      </div>

      <Dialog :open="confirmRegen" :title="t('nodes.regenerateTitle')" :description="t('nodes.regenerateBody')" @update:open="(o) => !o && !busy && (confirmRegen = false)">
        <template #actions>
          <Button :disabled="busy" @click="confirmRegen = false">{{ t('profile.cancel') }}</Button>
          <Button variant="danger" :disabled="busy" @click="regenerate">{{ t('nodes.regenerate') }}</Button>
        </template>
      </Dialog>
      <Dialog :open="!!command" :title="t('nodes.installTitle')" @update:open="(o) => !o && (command = '')">
        <InstallCommand :command="command" />
        <template #actions><Button @click="command = ''">{{ t('nodes.done') }}</Button></template>
      </Dialog>
      <Dialog
        :open="confirmDelete"
        :title="t('nodes.deleteTitle')"
        :description="t('nodes.deleteBody', { name: node.name })"
        @update:open="(o) => !o && !busy && (confirmDelete = false)"
      >
        <template #actions>
          <Button :disabled="busy" @click="confirmDelete = false">{{ t('profile.cancel') }}</Button>
          <Button variant="danger" :disabled="busy" @click="remove">{{ t('nodes.delete') }}</Button>
        </template>
      </Dialog>
    </template>
  </template>
</template>

<style scoped>
.head {
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
  margin: 4px 0 0;
  color: var(--muted);
}
.mono {
  font-family: var(--font-mono, ui-monospace, monospace);
  font-size: 0.75rem;
}
.muted {
  color: var(--muted);
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
.cards {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  gap: 16px;
}
.card {
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--panel);
  padding: 12px 16px;
}
.cards > .card,
.stack {
  display: grid;
  gap: 6px;
}
.stack p {
  margin: 0;
}
.danger-zone {
  border-color: var(--danger);
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
  min-width: 0;
  overflow-wrap: anywhere;
}
.row {
  display: flex;
  flex-wrap: wrap;
  gap: 8px 16px;
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
</style>
