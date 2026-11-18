export interface WritableScope {
  scope: 'org' | 'team' | 'user'
  teamId?: string
  teamName?: string
}

export interface EnvVar extends WritableScope {
  id: string
  name: string
  kind: 'env' | 'secret'
  value?: string
  hosts: string[]
  updatedAt: string
  updatedBy: { id: string; name: string }
  canEdit: boolean
  canDelete: boolean
}

type Translate = (key: string) => string

export const scopeKey = (s: WritableScope) => (s.scope === 'team' ? `team:${s.teamId}` : s.scope)
export const scopeRef = (s: WritableScope) => ({ scope: s.scope, ...(s.scope === 'team' ? { teamId: s.teamId } : {}) })
export const scopeLabel = (s: WritableScope, t: Translate) =>
  s.scope === 'team' ? `${t('env.scopeTeam')} ${s.teamName ?? ''}` : t(s.scope === 'org' ? 'env.scopeOrg' : 'env.scopePrivate')

const hostLabel = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/

export function parseHosts(text: string): string[] {
  return [...new Set(text.split(/[\s,]+/).filter(Boolean).map((h) => h.toLowerCase()))].sort()
}

export function invalidHost(hosts: string[]): string | undefined {
  return hosts.find((h) => {
    const labels = h.split('.')
    return h.length > 253 || labels.length < 2 || !labels.every((l) => l.length <= 63 && hostLabel.test(l)) || /^[0-9]+$/.test(labels.at(-1) ?? '')
  })
}
