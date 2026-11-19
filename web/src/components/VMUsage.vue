<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import { gb, type VmInfo } from '@/composables/nodes'

interface Sample {
  at: string
  cpuPercent: number
  memTotalMB: number
  memUsedMB: number
}

const props = defineProps<{ orgId: string; vm: VmInfo }>()
const { t } = useI18n()
const samples = ref<Sample[]>([])
const error = ref('')

const W = 240
const H = 40
const now = ref(Date.now())
const last = computed(() => samples.value.at(-1))
const live = computed(() => !!last.value && now.value - Date.parse(last.value.at) < 30000)
const hasRam = computed(() => !!last.value?.memTotalMB)

function line(values: number[], max: number) {
  if (values.length < 2) return ''
  return values.map((v, i) => `${((i / (values.length - 1)) * W).toFixed(1)},${(H - (Math.min(v, max) / max) * H).toFixed(1)}`).join(' ')
}
const cpuLine = computed(() => line(samples.value.map((s) => s.cpuPercent), 100))
const ramLine = computed(() => line(samples.value.map((s) => s.memUsedMB), last.value?.memTotalMB || 1))

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
  <h2>{{ t('usage.section') }}</h2>
  <p v-if="error" role="alert">{{ error }}</p>
  <p v-else-if="!last">{{ t('usage.none') }}</p>
  <p v-else-if="!live">{{ t('usage.stale') }}</p>
  <div v-else class="usage">
    <div>
      <strong>{{ t('usage.cpu') }} {{ Math.round(last.cpuPercent) }} %</strong>
      <svg :viewBox="`0 0 ${W} ${H}`" :width="W" :height="H" role="img" :aria-label="t('usage.cpu')"><polyline :points="cpuLine" fill="none" stroke="currentColor" stroke-width="1.5" /></svg>
    </div>
    <div>
      <strong>{{ t('usage.ram') }} <template v-if="hasRam">{{ gb(last.memUsedMB) }} / {{ gb(last.memTotalMB) }} GB</template><template v-else>–</template></strong>
      <svg v-if="hasRam" :viewBox="`0 0 ${W} ${H}`" :width="W" :height="H" role="img" :aria-label="t('usage.ram')"><polyline :points="ramLine" fill="none" stroke="currentColor" stroke-width="1.5" /></svg>
      <small v-else>{{ t('usage.noRam') }}</small>
    </div>
  </div>
</template>

<style scoped>
.usage {
  display: flex;
  flex-wrap: wrap;
  gap: 2rem;
}
.usage > div {
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
}
</style>
