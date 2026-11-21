import type { WritableScope } from './envScopes.ts'
import type { Series, Unit } from './telemetry.ts'

export type PanelUnit = 'none' | 'percent' | 'bytes' | 'bytes_per_second' | 'seconds' | 'requests_per_second'

export interface Panel {
  title: string
  type: 'timeseries' | 'stat'
  query: string
  unit: PanelUnit
  width: 'half' | 'full'
}

export interface Dashboard {
  id: string
  name: string
  scope: WritableScope['scope']
  ownerId: string | null
  panels: Panel[]
  createdBy: string
  createdAt: string
  updatedAt: string
  canEdit: boolean
}

export interface RangeResult {
  t: number[]
  series: { labels: Record<string, string>; values: (number | null)[] }[]
}

export interface InstantResult {
  series: { labels: Record<string, string>; value: number | null }[]
}

export const panelTypes: Panel['type'][] = ['timeseries', 'stat']
export const panelUnits = ['none', 'percent', 'bytes', 'bytes_per_second', 'seconds', 'requests_per_second'] as const
export const chartUnits: Record<PanelUnit, Unit> = {
  none: '1',
  percent: '%',
  bytes: 'By',
  bytes_per_second: 'By/s',
  seconds: 's',
  requests_per_second: 'req/s',
}

export const exampleQuery = 'sum(rate(system_cpu_time_seconds_total{state!="idle"}[5m])) by (observatory_vm_name)'
export const newPanel = (query = ''): Panel => ({ title: '', type: 'timeseries', query, unit: 'none', width: 'half' })

const hiddenLabels = new Set(['__name__', 'observatory_org', 'observatory_node', 'observatory_vm', 'observatory_vm_name'])
const MAX_LABEL = 60
const MAX_QUERY_LABEL = 40

function legendLabel(labels: Record<string, string>, panel: Pick<Panel, 'title' | 'query'>) {
  const parts = Object.keys(labels)
    .filter((k) => !hiddenLabels.has(k))
    .sort()
    .map((k) => `${k}=${labels[k]}`)
  if (labels.observatory_vm_name) parts.unshift(labels.observatory_vm_name)
  const s = parts.join(' ') || panel.title || labels.__name__ || panel.query.trim().slice(0, MAX_QUERY_LABEL)
  return s.length > MAX_LABEL ? `${s.slice(0, MAX_LABEL - 1)}…` : s
}

// Charts key lines by name, so series that differ only in hidden labels get a suffix.
export function legendLabels(labels: Record<string, string>[], panel: Pick<Panel, 'title' | 'query'>) {
  const seen = new Map<string, number>()
  return labels.map((l) => {
    const base = legendLabel(l, panel)
    const n = (seen.get(base) ?? 0) + 1
    seen.set(base, n)
    return n === 1 ? base : `${base} #${n}`
  })
}

export function toSeries(r: RangeResult, panel: Pick<Panel, 'title' | 'query'>): Series[] {
  const names = legendLabels(r.series.map((s) => s.labels), panel)
  return r.series.map((s, i) => ({ name: names[i] ?? '', values: s.values }))
}

// Round steps keep points aligned between refreshes; about 300 points per series.
const steps = [15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400]
export function stepFor(from: Date, to: Date) {
  const want = (to.getTime() - from.getTime()) / 1000 / 300
  return steps.find((s) => s >= want) ?? steps[steps.length - 1]!
}

export function metricMatch(prefix: string) {
  const re = prefix.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  return `{__name__=~"${re.replace(/[\\"]/g, '\\$&')}.*"}`
}

export function wordAt(text: string, cursor: number) {
  const word = /[a-zA-Z0-9_:]*$/.exec(text.slice(0, cursor))?.[0] ?? ''
  return { start: cursor - word.length, word }
}

const control = /[\u0000-\u001f\u007f-\u009f]/

// Mirrors the API's checks so the editor can point at the bad field before saving.
export function panelError(p: Panel): '' | 'title' | 'type' | 'query' | 'unit' | 'width' {
  if ([...p.title].length > 100 || control.test(p.title)) return 'title'
  if (!panelTypes.includes(p.type)) return 'type'
  if (!p.query.trim() || new TextEncoder().encode(p.query).length > 4096) return 'query'
  if (!(panelUnits as readonly string[]).includes(p.unit)) return 'unit'
  if (p.width !== 'half' && p.width !== 'full') return 'width'
  return ''
}

export const nameValid = (name: string) => {
  const n = [...name.trim()].length
  return n >= 1 && n <= 100 && !control.test(name)
}

export interface Team {
  id: string
  name: string
}

// teams: those the viewer may create in; private only for the creator (a move must not take a dashboard from its owner).
export const dashboardScopes = (teams: Team[], org: boolean, own: boolean): WritableScope[] => [
  ...(own ? [{ scope: 'user' as const }] : []),
  ...teams.map((x) => ({ scope: 'team' as const, teamId: x.id, teamName: x.name })),
  ...(org ? [{ scope: 'org' as const }] : []),
]

export const dashboardScope = (d: Dashboard, teams: Team[]): WritableScope =>
  d.scope === 'team' ? { scope: 'team', teamId: d.ownerId ?? '', teamName: teams.find((x) => x.id === d.ownerId)?.name ?? '' } : { scope: d.scope }

// Caps concurrent calls; dashboards share one so a full dashboard stays under the per-user query rate limit.
export function limiter(max: number) {
  let active = 0
  const waiting: (() => void)[] = []
  return async <T>(fn: () => Promise<T>): Promise<T> => {
    while (active >= max) await new Promise<void>((r) => waiting.push(r))
    active++
    try {
      return await fn()
    } finally {
      active--
      waiting.shift()?.()
    }
  }
}

export const panelQueue = limiter(6)

const panelIds = new WeakMap<Panel, number>()
let nextPanelId = 0

// Stable v-for key per raw panel object, kept by edit copies, so reordering, removing or starting an edit doesn't re-query other panels.
export function panelKey(p: Panel) {
  let id = panelIds.get(p)
  if (id === undefined) panelIds.set(p, (id = ++nextPanelId))
  return id
}

export function copyPanel(p: Panel) {
  const c = { ...p }
  panelIds.set(c, panelKey(p))
  return c
}

// ponytail: non-admins need one team lookup each; a writableScopes field on the list API (like env) if orgs get many teams.
export async function creatableTeams(getTeam: (team: string) => Promise<{ members: { id: string }[] }>, me: string, all: boolean, teams: Team[]): Promise<Team[]> {
  if (all) return teams
  const member = await Promise.all(
    teams.map((x) => getTeam(x.id).then((d) => d.members.some((m) => m.id === me), () => false)),
  )
  return teams.filter((_, i) => member[i])
}
