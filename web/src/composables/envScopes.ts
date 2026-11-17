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
