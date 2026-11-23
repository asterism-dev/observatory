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
  autoSchedule: '' | 'daily' | 'weekly'
  autoOnBaseChange: boolean
  autoError: string
  autoLastAt: string | null
  autoRevision: number | null
  autoNeedsApproval: boolean
  source: GithubSource | null
}

export interface GithubSource {
  repo: string
  ref: string
  path: string
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
  automatic: boolean
  queuedAt: string
  startedAt: string | null
  finishedAt: string | null
  digest: string
  error: string
  source: string
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

const autoOn = (schedule: string, onBaseChange: boolean) => (schedule !== '' && schedule !== 'off') || onBaseChange

// Turning automation on approves the revision on screen; later revisions by others need their own approval.
export function autoSettingsBody(image: Pick<Image, 'autoSchedule' | 'autoOnBaseChange' | 'recipeRevision'>, schedule: string, onBaseChange: boolean) {
  const body: { autoSchedule: string; autoOnBaseChange: boolean; approveRevision?: number } = {
    autoSchedule: schedule === 'off' ? '' : schedule,
    autoOnBaseChange: onBaseChange,
  }
  if (!autoOn(image.autoSchedule, image.autoOnBaseChange) && autoOn(schedule, onBaseChange) && image.recipeRevision !== null) body.approveRevision = image.recipeRevision
  return body
}

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

export const LOG_CAP = 2 << 20

export interface LogView {
  text: string
  truncated: boolean
}

// Keeps the last cap characters, cut at a line start, so huge logs stay renderable.
export function appendCapped(log: LogView, chunk: string, cap = LOG_CAP): LogView {
  const text = log.text + chunk
  if (text.length <= cap) return { text, truncated: log.truncated }
  const tail = text.slice(-cap)
  const nl = tail.indexOf('\n')
  return { text: nl >= 0 && nl < tail.length - 1 ? tail.slice(nl + 1) : tail, truncated: true }
}

export const formatSource = (s: GithubSource | null) => (s ? `${s.repo}${s.ref ? `@${s.ref}` : ''}${s.path ? `:${s.path}` : ''}` : '')

export function commitOf(source: string) {
  const m = /^github:(.+)@([0-9a-f]{40}):/.exec(source)
  return m ? { repo: m[1], sha: m[2] } : null
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
