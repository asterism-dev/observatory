<script setup lang="ts">
import uPlot from 'uplot'
import 'uplot/dist/uPlot.min.css'
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { formatValue, type Series, type Unit } from '@/composables/telemetry'
import { Card } from '@/ui'

const props = defineProps<{ title: string; unit: string; t: number[]; series: Series[]; max?: number; syncKey: string }>()
const emit = defineEmits<{ zoom: [from: number, to: number]; reset: []; hover: [t: number | null] }>()

const { locale } = useI18n()
const el = ref<HTMLElement | null>(null)
const tip = ref<{ left: number; flip: boolean; time: string; rows: { name: string; color: string; value: string }[] } | null>(null)
const hidden = new Set<string>()
const palette = ['--ws-blue', '--ws-green', '--ws-amber', '--ws-purple', '--ws-red', '--ws-teal', '--ws-pink', '--ws-slate']
const HEIGHT = 200
// Byte axes tick at powers of two (8 GB, 16 GB), not at decimal steps.
const byteIncrs = Array.from({ length: 50 }, (_, i) => 2 ** i)
let plot: uPlot | null = null
let colors: string[] = []

const fmt = (v: number | null | undefined) => (v == null ? '–' : formatValue(v, props.unit as Unit))
const latest = computed(() =>
  props.series
    .slice(0, 2)
    .map((s) => `${s.name} ${fmt(s.values.filter((v) => v != null).at(-1))}`)
    .join(' · '),
)
const data = (): uPlot.AlignedData => [props.t, ...props.series.map((s) => s.values)]

function showTip(u: uPlot) {
  const i = u.cursor.idx
  const ts = i == null ? undefined : props.t[i]
  if (i == null || ts == null || u.cursor.left == null || u.cursor.left < 0) {
    tip.value = null
    return emit('hover', null)
  }
  emit('hover', ts)
  tip.value = {
    left: u.over.offsetLeft + u.cursor.left,
    flip: u.cursor.left > u.over.clientWidth / 2,
    time: new Date(ts * 1000).toLocaleString(locale.value),
    rows: props.series.flatMap((s, k) => (u.series[k + 1]?.show ? [{ name: s.name, color: colors[k] ?? '', value: fmt(s.values[i]) }] : [])),
  }
}

function build() {
  plot?.destroy()
  if (!el.value) return
  const css = getComputedStyle(document.documentElement)
  const token = (name: string) => css.getPropertyValue(name).trim()
  colors = props.series.map((_, i) => token(palette[i % palette.length] ?? ''))
  const axis = { stroke: token('--muted'), grid: { stroke: token('--border'), width: 1 }, ticks: { stroke: token('--border'), width: 1 } }
  plot = new uPlot(
    {
      width: el.value.clientWidth,
      height: HEIGHT,
      cursor: { sync: { key: props.syncKey }, drag: { x: true, y: false, setScale: false } },
      legend: { live: false },
      scales: { y: { range: (_u, _min, max) => [0, props.max ?? (max > 0 ? max * 1.1 : 1)] } },
      axes: [axis, { ...axis, size: 70, incrs: props.unit === '%' ? undefined : byteIncrs, values: (_u, splits) => splits.map((v) => fmt(v)) }],
      series: [{}, ...props.series.map((s, i) => ({ label: s.name, stroke: colors[i], width: 1.5, show: !hidden.has(s.name) }))],
      hooks: {
        setCursor: [showTip],
        setSelect: [
          (u) => {
            if (u.select.width < 3) return
            emit('zoom', u.posToVal(u.select.left, 'x'), u.posToVal(u.select.left + u.select.width, 'x'))
            u.setSelect({ left: 0, top: 0, width: 0, height: 0 }, false)
          },
        ],
        setSeries: [
          (u, i) => {
            const s = i == null ? undefined : u.series[i]
            if (!s?.label) return
            if (s.show) hidden.delete(String(s.label))
            else hidden.add(String(s.label))
          },
        ],
      },
    },
    data(),
    el.value,
  )
  plot.over.addEventListener('dblclick', () => emit('reset'))
  plot.over.addEventListener('mouseleave', () => {
    tip.value = null
    emit('hover', null)
  })
}

watch(() => props.series.map((s) => s.name).join('\n'), build)
watch(
  () => [props.t, props.series],
  () => plot?.setData(data()),
)

const resize = new ResizeObserver(() => el.value && plot?.setSize({ width: el.value.clientWidth, height: HEIGHT }))
// Canvas colours are resolved once, so a theme switch needs a rebuild.
const theme = new MutationObserver(build)
onMounted(() => {
  build()
  if (el.value) resize.observe(el.value)
  theme.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] })
})
onBeforeUnmount(() => {
  resize.disconnect()
  theme.disconnect()
  plot?.destroy()
})
</script>

<template>
  <Card>
    <div class="head">
      <span class="title">{{ title }}</span>
      <span class="mono small muted">{{ latest }}</span>
    </div>
    <div class="plot">
      <div ref="el" />
      <div v-if="tip" class="tip mono" :style="{ left: `${tip.left}px` }" :class="{ flip: tip.flip }">
        <div class="muted">{{ tip.time }}</div>
        <div v-for="r in tip.rows" :key="r.name">
          <i :style="{ background: r.color }" />{{ r.name }}: <b>{{ r.value }}</b>
        </div>
      </div>
    </div>
  </Card>
</template>

<style scoped>
.head {
  display: flex;
  justify-content: space-between;
  align-items: baseline;
  gap: 8px;
}
.title {
  font-size: 0.75rem;
  letter-spacing: 0.06em;
  text-transform: uppercase;
}
.small {
  font-size: 0.7rem;
}
.plot {
  position: relative;
  min-width: 0;
}
.plot :deep(.u-legend) {
  font-family: var(--font-mono);
  font-size: 0.7rem;
  color: var(--text);
}
.plot :deep(.u-select) {
  background: color-mix(in srgb, var(--ws-blue) 20%, transparent);
}
.tip {
  position: absolute;
  top: 8px;
  z-index: 2;
  pointer-events: none;
  transform: translateX(10px);
  padding: 6px 8px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--panel);
  font-size: 0.7rem;
  white-space: nowrap;
}
.tip.flip {
  transform: translateX(calc(-100% - 10px));
}
.tip i {
  display: inline-block;
  width: 8px;
  height: 8px;
  margin-right: 5px;
  border-radius: 2px;
}
</style>
