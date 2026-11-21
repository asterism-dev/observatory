export type Preset = '1h' | '6h' | '24h' | '7d'
export const presets: Preset[] = ['1h', '6h', '24h', '7d']
const presetHours: Record<Preset, number> = { '1h': 1, '6h': 6, '24h': 24, '7d': 168 }

export type Unit = '%' | 'By' | 'By/s' | '1' | 's' | 'req/s'

export interface Series {
  name: string
  values: (number | null)[]
}

export interface Usage {
  t: number[]
  charts: Partial<Record<'cpu' | 'mem' | 'disk' | 'net', Series[]>>
  dropped?: Dropped
}

export type Dropped = Record<'logs' | 'series' | 'body', number>

export const droppedTotal = (d: Dropped | undefined) => (d ? d.logs + d.series + d.body : 0)

// now: the range ends at the current time and moves with it.
export type UsageRange = { from: Date; to: Date; now: boolean }

export interface LogLine {
  time: string
  level: string
  source: string
  stream?: string
  msg: string
  fields: Record<string, unknown>
}

export type LogFilter = { from: Date; to: Date; q: string; level: string; source?: string; stream?: string; fields: Record<string, string> }

export function rangeFor(preset: Preset, now: Date) {
  return { from: new Date(now.getTime() - presetHours[preset] * 3600e3), to: now }
}

// 3 significant digits with an SI suffix: 0.0123, 12.3, 1.23 M.
function compact(v: number) {
  const suffixes = ['', ' k', ' M', ' G', ' T']
  let i = 0
  while (Math.abs(v) >= 1000 && i < suffixes.length - 1) {
    v /= 1000
    i++
  }
  return `${Number(v.toPrecision(3))}${suffixes[i]}`
}

function duration(v: number) {
  const a = Math.abs(v)
  if (a === 0 || a >= 3600) return a === 0 ? '0 s' : `${compact(v / 3600)} h`
  if (a >= 60) return `${compact(v / 60)} min`
  if (a >= 1) return `${compact(v)} s`
  return a >= 1e-3 ? `${compact(v * 1e3)} ms` : `${compact(v * 1e6)} µs`
}

export function formatValue(v: number, unit: Unit) {
  if (unit === '%') return `${v.toFixed(1)} %`
  if (unit === '1') return compact(v)
  if (unit === 'req/s') return `${compact(v)} req/s`
  if (unit === 's') return duration(v)
  const units = ['B', 'KB', 'MB', 'GB']
  let i = 0
  while (Math.abs(v) >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(1)} ${units[i]}${unit === 'By/s' ? '/s' : ''}`
}

export function logParams(f: LogFilter) {
  const p = new URLSearchParams({ from: f.from.toISOString(), to: f.to.toISOString() })
  if (f.q) p.set('q', f.q)
  if (f.level) p.set('level', f.level)
  if (f.source) p.set('source', f.source)
  if (f.stream) p.set('stream', f.stream)
  for (const [k, v] of Object.entries(f.fields)) p.append('field', `${k}:${v}`)
  return p
}

const hubKey = (k: string) => k.startsWith('observatory.')

export function prettyLine(l: LogLine) {
  const fieldKeys = Object.keys(l.fields).sort((a, b) => Number(hubKey(a)) - Number(hubKey(b)) || a.localeCompare(b))
  const entries: [string, unknown, boolean][] = [
    ['time', l.time, false],
    ['level', l.level, false],
    ['source', l.source, false],
    ['msg', l.msg, false],
    ...fieldKeys.map((k): [string, unknown, boolean] => [k, l.fields[k], !hubKey(k)]),
  ]
  return [
    { key: '', text: '{', clickable: false },
    ...entries.map(([key, v, clickable], i) => ({
      key,
      text: `  ${JSON.stringify(key)}: ${JSON.stringify(v)}${i < entries.length - 1 ? ',' : ''}`,
      clickable,
    })),
    { key: '', text: '}', clickable: false },
  ]
}
