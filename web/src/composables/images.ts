import type { WritableScope } from './envScopes.ts'

export type ImageScope = 'org' | 'team' | 'user'

export interface Image {
  id: string
  name: string
  description: string
  ref: string
  repository?: string
  scope: ImageScope
  ownerId?: string
  ownerName?: string
  platform: boolean
  recipeRevision: number | null
  tags: Record<string, string>
  latestDigest: string
  sizeBytes: number
  lastBuildState: string
  pushError: string
  createdAt: string
  canEdit: boolean
  canBuild: boolean
}

export interface RecipeRef {
  kind: 'secret' | 'integration'
  scope: ImageScope
  teamId?: string
  name: string
}

export interface RecipeFile {
  path: string
  mode: number
  size: number
  content: string | null
}

export interface Recipe {
  revision: number | null
  files: RecipeFile[]
  refs: RecipeRef[]
}

export interface Build {
  id: string
  revision: number
  state: 'queued' | 'running' | 'succeeded' | 'failed' | 'cancelled'
  startedBy: string
  queuedAt: string
  startedAt: string | null
  finishedAt: string | null
  digest: string
  error: string
}

export interface ImageVersion {
  digest: string
  source: 'build' | 'push'
  revision: number | null
  buildId: string | null
  sizeBytes: number
  createdAt: string
  tags: string[]
}

export interface ImageDefault {
  configured: { ref: string; removed: boolean } | null
  effective: { ref: string; source: string; digest: string; imageId?: string }
}

type Translate = (key: string) => string

export const buildTones = { queued: 'warning', running: 'warning', succeeded: 'success', failed: 'danger', cancelled: 'muted' } as const
export const buildActive = (b: Pick<Build, 'state'>) => b.state === 'queued' || b.state === 'running'
export const shortDigest = (d: string) => d.replace(/^sha256:/, '').slice(0, 12)

export function refLabel(ref: RecipeRef, teamNames: Record<string, string>) {
  const scope = ref.scope === 'team' ? `team:${teamNames[ref.teamId ?? ''] ?? ref.teamId}` : ref.scope === 'user' ? 'private' : 'org'
  return `${ref.kind}:${scope}/${ref.name}`
}

export function buildDuration(b: Pick<Build, 'startedAt' | 'finishedAt'>, now = Date.now()) {
  if (!b.startedAt) return ''
  const sec = Math.max(0, Math.round(((b.finishedAt ? Date.parse(b.finishedAt) : now) - Date.parse(b.startedAt)) / 1000))
  if (sec < 60) return `${sec}s`
  if (sec < 3600) return `${Math.floor(sec / 60)}m ${sec % 60}s`
  return `${Math.floor(sec / 3600)}h ${Math.floor((sec % 3600) / 60)}m`
}

export const defaultSourceLabel = (source: string, t: Translate) => t(`images.source.${source}`)

export function formatBytes(n: number) {
  const units = ['B', 'KiB', 'MiB', 'GiB']
  let i = 0
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024
    i++
  }
  return `${i ? n.toFixed(1) : n} ${units[i]}`
}

// ponytail: offers every team; the hub refuses scopes the caller may not edit. Return writable scopes from the API if that confuses users.
export const imageScopes = (teams: { id: string; name: string }[]): WritableScope[] => [
  { scope: 'user' },
  { scope: 'org' },
  ...teams.map((x) => ({ scope: 'team' as const, teamId: x.id, teamName: x.name })),
]

export const imageScope = (img: Image): WritableScope => ({ scope: img.scope, teamId: img.ownerId, teamName: img.ownerName })
