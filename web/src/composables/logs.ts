import type { Preset } from './telemetry.ts'

export type GroupKind = 'vm' | 'node' | 'image'
export const groupKinds: GroupKind[] = ['vm', 'node', 'image']

export interface LogGroup {
  kind: GroupKind
  id: string
  name: string
  deleted: boolean
  streams: number
  lastAt: string
}

export interface LogStream {
  name: string
  lastAt: string
  build?: { startedAt: string | null; status: string }
}

export interface LogQueryResult {
  columns: string[]
  rows: Record<string, string>[]
  stats: boolean
}

const prefix: Record<GroupKind, string> = { vm: 'vms', node: 'nodes', image: 'builds' }

export const groupName = (g: Pick<LogGroup, 'kind' | 'name'>) => `${prefix[g.kind]}/${g.name}`
export const groupKey = (g: Pick<LogGroup, 'kind' | 'id'>) => `${g.kind}/${g.id}`
export const groupPath = (orgId: string, g: Pick<LogGroup, 'kind' | 'id'>) => `/orgs/${orgId}/logs/${g.kind}/${g.id}`
export const entriesUrl = (orgId: string, g: Pick<LogGroup, 'kind' | 'id'>) => `/api/orgs/${orgId}/logs/groups/${g.kind}/${g.id}/entries`

export function filterGroups(groups: LogGroup[], kind: GroupKind | 'all', search: string) {
  const q = search.trim().toLowerCase()
  return groups.filter((g) => (kind === 'all' || g.kind === kind) && (!q || groupName(g).toLowerCase().includes(q)))
}

export function queryBody(query: string, groups: Pick<LogGroup, 'kind' | 'id'>[], range: { from: Date; to: Date }) {
  return {
    query: query.trim() || '*',
    groups: groups.map(({ kind, id }) => ({ kind, id })),
    from: range.from.toISOString(),
    to: range.to.toISOString(),
    limit: 1000,
  }
}

export interface LogGroupList {
  retentionSeconds: number
  groups: LogGroup[]
}

export function retentionSpan(seconds: number): { unit: 'days' | 'hours'; n: number } {
  const hours = Math.max(1, Math.round(seconds / 3600))
  return hours % 24 === 0 ? { unit: 'days', n: hours / 24 } : { unit: 'hours', n: hours }
}

export const retentionRange = 'retention'

// Presets stop at 7 days; the extra option reaches back the whole retention.
export function rangeOptions(presets: string[], retentionSeconds: number, retentionLabel: string) {
  const out = presets.map((p) => ({ value: p, label: p }))
  if (retentionSeconds > 7 * 86400) out.push({ value: retentionRange, label: retentionLabel })
  return out
}

export function logRange(value: string, retentionSeconds: number, now: Date, preset: (p: Preset, now: Date) => { from: Date; to: Date }) {
  if (value === retentionRange) return { from: new Date(now.getTime() - retentionSeconds * 1000), to: now }
  return preset(value as Preset, now)
}
