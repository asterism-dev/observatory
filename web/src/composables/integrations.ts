import type { WritableScope } from './envScopes'

export type IntegrationKind = 'github' | 'claude' | 'linear' | 'gcloud'
export type IntegrationMode = 'proxy' | 'vm'

export interface ClaudeUsage {
  fiveHourPct: number | null
  fiveHourResetsAt: string | null
  weekPct: number | null
  weekResetsAt: string | null
  limitedUntil: string | null
  source: string
  at: string | null
}

export interface ClaudeAccount {
  id: string
  account: string
  source: 'oauth' | 'setup-token'
  position: number
  active: boolean
  status: 'ok' | 'expired'
  lastError: string
  usage: ClaudeUsage
}

export interface ClaudeLastSwitch {
  at: string
  fromId: string
  from: string
  toId: string
  to: string
}

export interface Integration extends WritableScope {
  id: string
  kind: IntegrationKind
  source: string
  mode: IntegrationMode
  account: string
  quotaProject?: string
  providerExpiresAt: string | null
  status: 'ok' | 'expired'
  lastError: string
  wasInVm: boolean
  canEdit: boolean
  canDelete: boolean
  accounts?: ClaudeAccount[]
  lastSwitch?: ClaudeLastSwitch | null
  autoSwitch: boolean
}

export interface IntegrationScope extends WritableScope {
  connected: IntegrationKind[]
}

export const kindLabels: Record<IntegrationKind, string> = { github: 'GitHub', claude: 'Claude', linear: 'Linear', gcloud: 'gcloud' }
export const defaultSource: Record<IntegrationKind, string> = { github: 'pat', claude: 'setup-token', linear: 'api-key', gcloud: 'adc' }

export const tokenPages: Record<IntegrationKind, string> = {
  github: 'https://github.com/settings/tokens',
  claude: 'https://claude.ai/settings/claude-code',
  linear: 'https://linear.app/settings/account/security',
  gcloud: 'https://myaccount.google.com/permissions',
}

export const daysLeft = (v: Integration) => (v.providerExpiresAt ? Math.max(0, Math.ceil((Date.parse(v.providerExpiresAt) - Date.now()) / 86_400_000)) : null)
export const statusTone = (v: Integration) => (v.status === 'expired' ? 'danger' : (daysLeft(v) ?? Infinity) <= 7 ? 'warning' : 'success')
export const statusLabel = (v: Integration, t: (key: string, args?: Record<string, unknown>) => string) =>
  v.status === 'expired' ? t('integrations.expired') : daysLeft(v) === null ? t('integrations.ok') : t('integrations.okExpires', { n: daysLeft(v) })
export const multiAccount = (v: Integration) => v.kind === 'claude' && v.scope === 'user' && v.mode === 'proxy' && !!v.accounts?.length
export const shownAccount = (v: Integration) => v.accounts?.find((a) => a.active)?.account || v.account

export const quotaProjectPattern = /^([a-z][a-z0-9-]{4,28}[a-z0-9])?$/
