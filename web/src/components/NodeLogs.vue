<script setup lang="ts">
import { Button, Select, TextInput } from '@asterism-dev/lumen'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import { logParams, prettyLine, type LogFilter, type LogLine, type UsageRange } from '@/composables/telemetry'
import { Card, Section } from '@/ui'

const props = defineProps<{ orgId: string; nodeId: string; range: UsageRange; hoverAt: number | null }>()

const { t, locale } = useI18n()
const q = ref('')
const level = ref('all')
const source = ref('all')
const fields = ref<Record<string, string>>({})
const live = ref(true)
const lines = ref<LogLine[]>([])
const open = ref(new Set<string>())
const error = ref('')
const maxLines = 2000
let seen = new Set<string>()
let stream: EventSource | null = null
let debounce = 0
// Bumped by every load and on unmount, so a load that finishes late neither shows its lines nor opens a stream.
let generation = 0
const linesEl = ref<HTMLElement | null>(null)

const levels = computed(() => [{ value: 'all', label: t('telemetry.allLevels') }, ...['error', 'warn', 'info', 'debug'].map((v) => ({ value: v, label: v }))])
const sources = computed(() => [{ value: 'all', label: t('telemetry.allSources') }, ...['observatory-node', 'firecracker'].map((v) => ({ value: v, label: v }))])
const filter = computed<LogFilter>(() => ({
  from: props.range.from,
  to: props.range.to,
  q: q.value.trim(),
  level: level.value === 'all' ? '' : level.value,
  source: source.value === 'all' ? '' : source.value,
  fields: fields.value,
}))
const following = computed(() => live.value && props.range.now)
const base = () => `/api/orgs/${props.orgId}/nodes/${props.nodeId}/logs`
const lineKey = (l: LogLine) => JSON.stringify(l)
const time = (iso: string) =>
  new Date(iso).toLocaleString(locale.value, { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' })

function stop() {
  stream?.close()
  stream = null
}

function tail(f: LogFilter) {
  const p = logParams(f)
  p.delete('from')
  p.delete('to')
  stop()
  stream = new EventSource(`${base()}/tail?${p}`)
  // The stream may end without notice; EventSource reconnects and the dedupe drops lines already shown.
  stream.onmessage = (e) => {
    const l = JSON.parse(e.data) as LogLine
    const k = lineKey(l)
    if (seen.has(k)) return
    seen.add(k)
    lines.value = [l, ...lines.value].slice(0, maxLines)
  }
  stream.onerror = () => {
    if (stream?.readyState !== EventSource.CLOSED) return
    stop()
    error.value = t('telemetry.streamFailed')
  }
}

async function load() {
  const gen = ++generation
  stop()
  // range.to of a live range is only set on preset changes; history must reach now or the tail leaves a gap.
  const f = props.range.now ? { ...filter.value, to: new Date() } : filter.value
  try {
    const res = await api<{ lines: LogLine[] }>(`${base()}?${logParams(f)}`)
    if (gen !== generation) return
    lines.value = res.lines
    seen = new Set(res.lines.map(lineKey))
    error.value = ''
  } catch (e) {
    if (gen === generation) error.value = e instanceof Error ? e.message : String(e)
    return
  }
  if (following.value) tail(f)
}

watch([filter, following], () => {
  window.clearTimeout(debounce)
  debounce = window.setTimeout(load, 250)
}, { immediate: true })
onBeforeUnmount(() => {
  window.clearTimeout(debounce)
  generation++
  stop()
})

// Toggled on the DOM directly: hover moves fast and re-rendering every row per move is wasted work.
watch(
  () => props.hoverAt,
  (at) => {
    const span = (props.range.to.getTime() - props.range.from.getTime()) / 80
    linesEl.value?.querySelectorAll<HTMLElement>('.ln').forEach((r) => r.classList.toggle('mark', at != null && Math.abs(Number(r.dataset.t) - at * 1000) < span))
  },
)

function toggle(k: string) {
  if (!open.value.delete(k)) open.value.add(k)
}

function addField(l: LogLine, key: string) {
  fields.value = { ...fields.value, [key]: String(l.fields[key]) }
}

function removeField(key: string) {
  const { [key]: _, ...rest } = fields.value
  fields.value = rest
}
</script>

<template>
  <Section :label="t('telemetry.logs')">
    <Card>
      <div class="bar">
        <TextInput v-model="q" type="search" :placeholder="t('telemetry.search')" :aria-label="t('telemetry.search')" />
        <Select v-model="level" :options="levels" :aria-label="t('telemetry.level')" />
        <Select v-model="source" :options="sources" :aria-label="t('telemetry.source')" />
        <Button v-if="range.now" variant="ghost" size="sm" class="live mono" :class="{ paused: !live }" @click="live = !live">
          {{ live ? `● ${t('telemetry.live')}` : `❚❚ ${t('telemetry.paused')}` }}
        </Button>
      </div>
      <div v-if="Object.keys(fields).length" class="chips">
        <button v-for="(v, k) in fields" :key="k" type="button" class="chip mono" :title="t('telemetry.removeFilter')" @click="removeField(String(k))">
          {{ k }}={{ v }} ✕
        </button>
      </div>
      <p v-if="error" role="alert">{{ error }}</p>
      <div ref="linesEl" class="lines mono">
        <p v-if="!lines.length" class="muted empty">{{ t('telemetry.noLines') }}</p>
        <template v-for="l in lines" :key="lineKey(l)">
          <div class="ln" :data-t="Date.parse(l.time)" role="button" tabindex="0" :aria-expanded="open.has(lineKey(l))" @click="toggle(lineKey(l))" @keydown.enter="toggle(lineKey(l))">
            <span class="muted">{{ open.has(lineKey(l)) ? '▾' : '▸' }} {{ time(l.time) }}</span>
            <span :class="`lv-${l.level}`">{{ l.level.toUpperCase() }}</span>
            <span class="src muted">{{ l.source }}</span>
            <span class="msg">{{ l.msg }}</span>
          </div>
          <pre v-if="open.has(lineKey(l))" class="json"><template v-for="(p, i) in prettyLine(l)" :key="i"><span
            v-if="p.clickable"
            class="kv"
            role="button"
            tabindex="0"
            :title="t('telemetry.filterBy')"
            @click="addField(l, p.key)"
            @keydown.enter="addField(l, p.key)"
          >{{ p.text }}</span><template v-else>{{ p.text }}</template>{{ '\n' }}</template></pre>
        </template>
      </div>
    </Card>
  </Section>
</template>

<style scoped>
.bar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
.bar > :first-child {
  flex: 1;
  min-width: 160px;
}
.bar > :not(:first-child) {
  flex: none;
  width: auto;
}
.live {
  color: var(--success);
  font-size: 0.7rem;
  letter-spacing: 0.08em;
  text-transform: uppercase;
}
.live.paused {
  color: var(--muted);
}
.chips {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}
.chip {
  border: 0;
  border-radius: 10px;
  padding: 2px 8px;
  font-size: 0.7rem;
  color: var(--text);
  background: color-mix(in srgb, var(--ws-blue) 20%, transparent);
  cursor: pointer;
}
.lines {
  max-height: 360px;
  overflow: auto;
  font-size: 0.72rem;
}
.ln {
  display: grid;
  grid-template-columns: 150px 52px 130px 1fr;
  gap: 8px;
  padding: 1px 4px;
  cursor: pointer;
}
.ln:hover {
  background: var(--select);
}
.ln.mark {
  background: color-mix(in srgb, var(--ws-blue) 15%, transparent);
}
.src,
.msg {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.lv-error {
  color: var(--danger);
}
.lv-warn {
  color: var(--warning);
}
.lv-info {
  color: var(--ws-blue);
}
.lv-debug {
  color: var(--muted);
}
.json {
  margin: 2px 4px 6px 162px;
  padding: 8px 10px;
  border-radius: var(--radius);
  background: var(--select);
  overflow: auto;
}
.kv {
  cursor: pointer;
  border-radius: 3px;
}
.kv:hover {
  background: color-mix(in srgb, var(--ws-blue) 20%, transparent);
}
.empty,
p {
  margin: 0;
}
@media (max-width: 767.98px) {
  .ln {
    grid-template-columns: auto auto 1fr;
  }
  .ln .msg {
    grid-column: 1 / -1;
    white-space: normal;
  }
  .json {
    margin-left: 4px;
  }
}
</style>
