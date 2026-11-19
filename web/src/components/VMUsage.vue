<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import { gb, type VmInfo } from '@/composables/nodes'
import { Card, Section } from '@/ui'

interface Sample {
  at: string
  cpuPercent: number
  memTotalMB: number
  memUsedMB: number
  diskUsedMB: number
}

const props = defineProps<{ orgId: string; vm: VmInfo }>()
const { t } = useI18n()
const samples = ref<Sample[]>([])
const error = ref('')

const W = 300
const H = 100
const now = ref(Date.now())
const last = computed(() => samples.value.at(-1))
const live = computed(() => !!last.value && now.value - Date.parse(last.value.at) < 30000)
const minutes = computed(() => {
  const first = samples.value[0]
  return first && last.value && first !== last.value ? Math.max(1, Math.round((Date.parse(last.value.at) - Date.parse(first.at)) / 60000)) : 0
})

function line(values: number[], max: number) {
  if (values.length < 2 || !max) return ''
  return values.map((v, i) => `${((i / (values.length - 1)) * W).toFixed(1)},${(H - (Math.min(v, max) / max) * (H - 2) - 1).toFixed(1)}`).join(' ')
}
const pct = (v: number, max: number) => (max ? (v / max) * 100 : null)

const cards = computed(() => {
  const s = samples.value
  const l = last.value
  const diskMB = props.vm.diskGB * 1024
  return [
    { key: 'cpu', label: t('usage.cpu'), detail: l ? `${((l.cpuPercent / 100) * props.vm.cpus).toFixed(1)} / ${props.vm.cpus} vCPU` : '', percent: l ? l.cpuPercent : null, points: line(s.map((x) => x.cpuPercent), 100) },
    l?.memTotalMB
      ? { key: 'mem', label: t('usage.mem'), detail: `${gb(l.memUsedMB)} / ${gb(l.memTotalMB)} GB`, percent: pct(l.memUsedMB, l.memTotalMB), points: line(s.map((x) => x.memUsedMB), l.memTotalMB) }
      : { key: 'mem', label: t('usage.mem'), detail: l ? t('usage.noRam') : '', percent: null, points: '' },
    { key: 'dsk', label: t('usage.dsk'), detail: l ? `${gb(l.diskUsedMB)} / ${props.vm.diskGB} GB` : '', percent: l ? pct(l.diskUsedMB, diskMB) : null, points: line(s.map((x) => x.diskUsedMB), diskMB) },
  ]
})

async function load() {
  const id = props.vm.id
  try {
    const res = await api<{ samples: Sample[] }>(`/api/orgs/${props.orgId}/vms/${id}/metrics`)
    if (id !== props.vm.id) return
    samples.value = res.samples
    now.value = Date.now()
    error.value = ''
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

const refresh = () => document.visibilityState === 'visible' && load()
const timer = window.setInterval(refresh, 10000)
document.addEventListener('visibilitychange', refresh)
onBeforeUnmount(() => {
  window.clearInterval(timer)
  document.removeEventListener('visibilitychange', refresh)
})

watch(
  () => props.vm.id,
  () => {
    samples.value = []
    load()
  },
  { immediate: true },
)
</script>

<template>
  <Section :label="t('usage.window', { n: minutes || 10 })">
    <template #actions><span v-if="live" class="live mono">{{ t('usage.live') }}</span></template>
    <p v-if="error" role="alert">{{ error }}</p>
    <p v-else-if="!last" class="muted">{{ t('usage.none') }}</p>
    <p v-else-if="!live" class="muted">{{ t('usage.stale') }}</p>
    <template v-if="last">
      <div class="cards">
        <Card v-for="c in cards" :key="c.key">
          <div class="card-head">
            <div>
              <div class="mono small">{{ c.label }}</div>
              <div class="mono small muted">{{ c.detail }}</div>
            </div>
            <strong>{{ c.percent === null ? '–' : `${Math.round(c.percent)}%` }}</strong>
          </div>
          <svg :viewBox="`0 0 ${W} ${H}`" preserveAspectRatio="none" role="img" :aria-label="c.label">
            <line v-for="y in [25, 50, 75]" :key="y" x1="0" :x2="W" :y1="y" :y2="y" class="grid" />
            <polyline v-if="c.points" :points="c.points" fill="none" class="plot" vector-effect="non-scaling-stroke" />
          </svg>
          <div class="axis mono small muted">
            <span>{{ t('usage.ago', { n: minutes || 10 }) }}</span>
            <span>{{ t('usage.now') }}</span>
          </div>
        </Card>
      </div>
      <p class="mono small muted">{{ t('usage.diskHint') }}</p>
    </template>
  </Section>
</template>

<style scoped>
.live {
  font-size: 0.7rem;
  letter-spacing: 0.08em;
  text-transform: uppercase;
  color: var(--success);
}
.cards {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
  gap: 16px;
}
.card-head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 8px;
}
svg {
  display: block;
  width: 100%;
  height: 100px;
}
.grid {
  stroke: var(--border);
  stroke-width: 0.5;
}
.plot {
  stroke: var(--success);
  stroke-width: 1.5;
}
.axis {
  display: flex;
  justify-content: space-between;
}
.small {
  font-size: 0.7rem;
}
p {
  margin: 0;
}
</style>
