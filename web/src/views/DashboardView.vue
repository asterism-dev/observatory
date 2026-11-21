<script setup lang="ts">
import { Button } from '@asterism-dev/lumen'
import { CheckCircle2, Circle, LayoutDashboard } from '@lucide/vue'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api, ApiError } from '@/api'
import AddNodeDialog from '@/components/AddNodeDialog.vue'
import VMList from '@/components/VMList.vue'
import { guideSteps, sshTarget, tiles, type DashboardInput } from '@/composables/dashboard'
import type { VmInfo } from '@/composables/nodes'
import { useMe } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'
import { Card, CommandBox, Eyebrow, Page, Section } from '@/ui'

type ListKey = 'nodes' | 'vms' | 'disks' | 'devices'

const { t } = useI18n()
const { org, can } = useOrg()
const { me } = useMe()

const input = ref<DashboardInput | null>(null)
const vmList = ref<VmInfo[] | null>(null)
const errors = ref<Partial<Record<ListKey | 'sshKeys', string>>>({})
const hidden = ref(false)
const addNodeOpen = ref(false)

const hiddenKey = (id: string) => `obs-guide-hidden:${id}`

function readHidden(id: string): boolean {
  try {
    return localStorage.getItem(hiddenKey(id)) === '1'
  } catch {
    return false
  }
}

function hide() {
  hidden.value = true
  try {
    if (org.value) localStorage.setItem(hiddenKey(org.value.id), '1')
  } catch {
    // Storage blocked: hidden for this visit only.
  }
}

async function load() {
  if (!org.value || !me.value) return
  const id = org.value.id
  const [nodes, vms, disks, devices, keys] = await Promise.allSettled([
    api<DashboardInput['nodes']>(`/api/orgs/${id}/nodes`),
    api<{ vms: VmInfo[] }>(`/api/orgs/${id}/vms`).then((r) => r.vms),
    api<{ disks: NonNullable<DashboardInput['disks']> }>(`/api/orgs/${id}/disks`).then((r) => r.disks),
    api<DashboardInput['devices']>(`/api/orgs/${id}/devices`),
    api<unknown[]>('/api/me/ssh-keys'),
  ])
  if (id !== org.value?.id) return
  const errs: typeof errors.value = {}
  // 403 hides the tile; other failures show it with an error line.
  function value<T>(key: ListKey | 'sshKeys', r: PromiseSettledResult<T>): T | null {
    if (r.status === 'fulfilled') return r.value
    if (!(r.reason instanceof ApiError && r.reason.status === 403)) errs[key] = t('dashboard.loadFailed')
    return null
  }
  const vmRows = value('vms', vms)
  input.value = {
    me: me.value.user.id,
    canCreateNode: can('compute.nodes.create'),
    nodes: value('nodes', nodes),
    vms: vmRows,
    disks: value('disks', disks),
    devices: value('devices', devices),
    sshKeys: value('sshKeys', keys)?.length ?? 0,
    now: Date.now(),
  }
  vmList.value = vmRows
  errors.value = errs
}

watch(
  () => org.value?.id,
  (id) => {
    input.value = null
    vmList.value = null
    errors.value = {}
    hidden.value = id ? readHidden(id) : false
    load()
  },
  { immediate: true },
)

const steps = computed(() => (input.value ? guideSteps(input.value) : []))
const showGuide = computed(() => !hidden.value && steps.value.some((s) => !s.done))
const target = computed(() => (input.value ? sshTarget(input.value) : null))
const sshCommand = computed(() => `ssh dev@${target.value}.internal`)
const summary = computed(() => (input.value ? tiles(input.value) : {}))
const hasAnything = computed(() => {
  const i = input.value
  return !!i && [i.nodes, i.vms, i.disks, i.devices].some((l) => l && l.length > 0)
})
const base = computed(() => `/orgs/${org.value?.id}`)
const myVms = computed(() => (vmList.value ?? []).filter((v) => v.owner.kind === 'user' && v.owner.id === me.value?.user.id))
</script>

<template>
  <Page v-if="org" :icon="LayoutDashboard" :title="t('nav.dashboard')" :subtitle="t('dashboard.subtitle')">
    <Card v-if="showGuide" class="guide">
      <div class="guide__head">
        <div class="guide__title">
          <Eyebrow accent>{{ t('dashboard.guide.label') }}</Eyebrow>
          <h2>{{ t('dashboard.guide.title') }}</h2>
        </div>
        <Button size="sm" variant="ghost" @click="hide">{{ t('dashboard.guide.hide') }}</Button>
      </div>
      <ol>
        <li v-for="s in steps" :key="s.id" :class="{ done: s.done }">
          <CheckCircle2 v-if="s.done" :size="18" class="icon icon--done" />
          <Circle v-else :size="18" class="icon" />
          <div class="step">
            <span>{{ t(`dashboard.step.${s.id}`) }}</span>
            <template v-if="!s.done">
              <template v-if="s.id === 'node'">
                <Button v-if="can('compute.nodes.create')" size="sm" variant="accent" @click="addNodeOpen = true">{{ t('nodes.add') }}</Button>
                <small v-else class="muted">{{ t('dashboard.step.nodeAsk') }}</small>
              </template>
              <router-link v-else-if="s.id === 'sshKey'" to="/profile">{{ t('nav.profile') }}</router-link>
              <router-link v-else-if="s.id === 'device'" :to="`${base}/devices`">{{ t('devices.add') }}</router-link>
              <router-link v-else-if="s.id === 'vm'" :to="`${base}/vms`">{{ t('nav.vms') }}</router-link>
              <template v-else-if="s.id === 'ssh'">
                <CommandBox v-if="target" class="step__command" :command="sshCommand" />
                <small v-else class="muted">{{ t('dashboard.step.sshHint') }}</small>
              </template>
            </template>
          </div>
        </li>
      </ol>
    </Card>
    <Section v-if="hasAnything || Object.keys(errors).length" :label="t('dashboard.overview')">
      <div class="tiles">
        <router-link v-if="summary.vms || errors.vms" :to="`${base}/vms`" class="tile">
          <Card>
            <span class="muted">{{ t('nav.vms') }}</span>
            <p v-if="errors.vms" role="alert">{{ errors.vms }}</p>
            <template v-else-if="summary.vms">
              <p><strong>{{ summary.vms.running }}</strong> {{ t('dashboard.tile.running') }}</p>
              <p class="muted">{{ summary.vms.stopped }} {{ t('dashboard.tile.stopped') }} · {{ summary.vms.other }} {{ t('dashboard.tile.other') }}</p>
            </template>
          </Card>
        </router-link>
        <router-link v-if="summary.nodes || errors.nodes" :to="`${base}/nodes`" class="tile">
          <Card>
            <span class="muted">{{ t('nav.nodes') }}</span>
            <p v-if="errors.nodes" role="alert">{{ errors.nodes }}</p>
            <template v-else-if="summary.nodes">
              <p><strong>{{ summary.nodes.online }}</strong> {{ t('dashboard.tile.online') }}</p>
              <p class="muted">{{ summary.nodes.offline }} {{ t('dashboard.tile.offline') }}</p>
            </template>
          </Card>
        </router-link>
        <router-link v-if="summary.disks || errors.disks" :to="`${base}/disks`" class="tile">
          <Card>
            <span class="muted">{{ t('nav.disks') }}</span>
            <p v-if="errors.disks" role="alert">{{ errors.disks }}</p>
            <template v-else-if="summary.disks">
              <p><strong>{{ summary.disks.count }}</strong> · {{ summary.disks.totalGB }} GB</p>
              <p class="muted">{{ summary.disks.attached }} {{ t('dashboard.tile.attached') }}</p>
            </template>
          </Card>
        </router-link>
        <router-link v-if="summary.devices || errors.devices" :to="`${base}/devices`" class="tile">
          <Card>
            <span class="muted">{{ t('nav.devices') }}</span>
            <p v-if="errors.devices" role="alert">{{ errors.devices }}</p>
            <template v-else-if="summary.devices">
              <p><strong>{{ summary.devices.connected }}</strong> {{ t('dashboard.tile.connected') }}</p>
              <p class="muted">{{ summary.devices.total }} {{ t('dashboard.tile.total') }}</p>
            </template>
          </Card>
        </router-link>
      </div>
    </Section>
    <Section v-if="vmList" :label="t('dashboard.yourVms')">
      <VMList :org-id="org.id" :vms="myVms" :empty="t('dashboard.noOwnVms')" @changed="load" />
    </Section>
    <AddNodeDialog v-model:open="addNodeOpen" :org-id="org.id" @created="load" />
  </Page>
</template>

<style scoped>
.guide {
  border-color: color-mix(in srgb, var(--accent) 40%, var(--border));
}
.guide__head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 8px;
}
.guide__title {
  display: grid;
  gap: 6px;
}
h2 {
  margin: 0;
  font-size: 1.15rem;
}
ol {
  list-style: none;
  margin: 0;
  padding: 0;
  display: grid;
  gap: 10px;
}
li {
  display: flex;
  align-items: flex-start;
  gap: 8px;
}
li.done .step > span {
  color: var(--muted);
}
.icon {
  flex: none;
  margin-top: 2px;
  color: var(--muted);
}
.icon--done {
  color: var(--accent);
}
.step {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 4px 12px;
  min-width: 0;
}
.step__command {
  flex: 1 1 320px;
}
.tiles {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
  gap: 16px;
}
.tile {
  color: var(--text);
  text-decoration: none;
}
.tile > .card {
  height: 100%;
  gap: 2px;
}
.tile:hover > .card {
  border-color: color-mix(in srgb, var(--accent) 60%, var(--border));
}
.tile strong {
  font-size: 1.4rem;
}
</style>
