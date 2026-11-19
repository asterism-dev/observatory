<script setup lang="ts">
import { Button } from '@asterism-dev/lumen'
import { CheckCircle2, Circle, Copy } from '@lucide/vue'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api, ApiError } from '@/api'
import AddNodeDialog from '@/components/AddNodeDialog.vue'
import { guideSteps, sshTarget, tiles, type DashboardInput } from '@/composables/dashboard'
import { useMe } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'

type ListKey = 'nodes' | 'vms' | 'disks' | 'devices'

const { t } = useI18n()
const { org, can } = useOrg()
const { me } = useMe()

const input = ref<DashboardInput | null>(null)
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
    api<{ vms: NonNullable<DashboardInput['vms']> }>(`/api/orgs/${id}/vms`).then((r) => r.vms),
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
  input.value = {
    me: me.value.user.id,
    canCreateNode: can('node:create'),
    nodes: value('nodes', nodes),
    vms: value('vms', vms),
    disks: value('disks', disks),
    devices: value('devices', devices),
    sshKeys: value('sshKeys', keys)?.length ?? 0,
    now: Date.now(),
  }
  errors.value = errs
}

watch(
  () => org.value?.id,
  (id) => {
    input.value = null
    errors.value = {}
    hidden.value = id ? readHidden(id) : false
    load()
  },
  { immediate: true },
)

const steps = computed(() => (input.value ? guideSteps(input.value) : []))
const showGuide = computed(() => !hidden.value && steps.value.some((s) => !s.done))
const target = computed(() => (input.value ? sshTarget(input.value) : null))
const sshCommand = computed(() => `ssh ubuntu@${target.value}.internal`)
const summary = computed(() => (input.value ? tiles(input.value) : {}))
const hasAnything = computed(() => {
  const i = input.value
  return !!i && [i.nodes, i.vms, i.disks, i.devices].some((l) => l && l.length > 0)
})
const base = computed(() => `/orgs/${org.value?.id}`)
const copy = () => navigator.clipboard.writeText(sshCommand.value)
</script>

<template>
  <template v-if="org">
    <h1>{{ t('nav.dashboard') }}</h1>
    <section v-if="showGuide" class="card guide">
      <div class="guide__head">
        <h2>{{ t('dashboard.guide.title') }}</h2>
        <Button size="sm" @click="hide">{{ t('dashboard.guide.hide') }}</Button>
      </div>
      <ol>
        <li v-for="s in steps" :key="s.id" :class="{ done: s.done }">
          <CheckCircle2 v-if="s.done" :size="18" class="icon icon--done" />
          <Circle v-else :size="18" class="icon" />
          <div class="step">
            <span>{{ t(`dashboard.step.${s.id}`) }}</span>
            <template v-if="!s.done">
              <template v-if="s.id === 'node'">
                <Button v-if="can('node:create')" size="sm" variant="accent" @click="addNodeOpen = true">{{ t('nodes.add') }}</Button>
                <small v-else>{{ t('dashboard.step.nodeAsk') }}</small>
              </template>
              <router-link v-else-if="s.id === 'sshKey'" to="/profile">{{ t('nav.profile') }}</router-link>
              <router-link v-else-if="s.id === 'device'" :to="`${base}/devices`">{{ t('devices.add') }}</router-link>
              <router-link v-else-if="s.id === 'vm'" :to="`${base}/vms`">{{ t('nav.vms') }}</router-link>
              <template v-else-if="s.id === 'ssh'">
                <span v-if="target" class="cmd">
                  <code>{{ sshCommand }}</code>
                  <Button size="sm" :aria-label="t('nodes.copy')" @click="copy"><Copy :size="14" /></Button>
                </span>
                <small v-else>{{ t('dashboard.step.sshHint') }}</small>
              </template>
            </template>
          </div>
        </li>
      </ol>
    </section>
    <div v-if="hasAnything || Object.keys(errors).length" class="tiles">
      <router-link v-if="summary.vms || errors.vms" :to="`${base}/vms`" class="card tile">
        <h2>{{ t('nav.vms') }}</h2>
        <p v-if="errors.vms" role="alert">{{ errors.vms }}</p>
        <template v-else-if="summary.vms">
          <p><strong>{{ summary.vms.running }}</strong> {{ t('dashboard.tile.running') }}</p>
          <p>{{ summary.vms.stopped }} {{ t('dashboard.tile.stopped') }} · {{ summary.vms.other }} {{ t('dashboard.tile.other') }}</p>
        </template>
      </router-link>
      <router-link v-if="summary.nodes || errors.nodes" :to="`${base}/nodes`" class="card tile">
        <h2>{{ t('nav.nodes') }}</h2>
        <p v-if="errors.nodes" role="alert">{{ errors.nodes }}</p>
        <template v-else-if="summary.nodes">
          <p><strong>{{ summary.nodes.online }}</strong> {{ t('dashboard.tile.online') }}</p>
          <p>{{ summary.nodes.offline }} {{ t('dashboard.tile.offline') }}</p>
        </template>
      </router-link>
      <router-link v-if="summary.disks || errors.disks" :to="`${base}/disks`" class="card tile">
        <h2>{{ t('nav.disks') }}</h2>
        <p v-if="errors.disks" role="alert">{{ errors.disks }}</p>
        <template v-else-if="summary.disks">
          <p><strong>{{ summary.disks.count }}</strong> · {{ summary.disks.totalGB }} GB</p>
          <p>{{ summary.disks.attached }} {{ t('dashboard.tile.attached') }}</p>
        </template>
      </router-link>
      <router-link v-if="summary.devices || errors.devices" :to="`${base}/devices`" class="card tile">
        <h2>{{ t('nav.devices') }}</h2>
        <p v-if="errors.devices" role="alert">{{ errors.devices }}</p>
        <template v-else-if="summary.devices">
          <p><strong>{{ summary.devices.connected }}</strong> {{ t('dashboard.tile.connected') }}</p>
          <p>{{ summary.devices.total }} {{ t('dashboard.tile.total') }}</p>
        </template>
      </router-link>
    </div>
    <AddNodeDialog v-model:open="addNodeOpen" :org-id="org.id" @created="load" />
  </template>
</template>

<style scoped>
.card {
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--panel);
  padding: 12px 16px;
}
.guide {
  margin-bottom: 16px;
}
.guide__head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 8px;
}
h2 {
  margin: 0 0 8px;
  font-size: 1rem;
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
}
.cmd {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.tiles {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(180px, 1fr));
  gap: 12px;
}
.tile {
  color: var(--text);
  text-decoration: none;
}
.tile:hover {
  border-color: var(--accent);
}
.tile p {
  margin: 2px 0;
}
.tile strong {
  font-size: 1.4rem;
}
</style>
