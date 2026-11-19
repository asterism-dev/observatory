import type { WritableScope } from './envScopes'

export type IntegrationKind = 'github' | 'claude' | 'linear'
export type IntegrationMode = 'proxy' | 'vm'

export interface Integration extends WritableScope {
  id: string
  kind: IntegrationKind
  source: string
  mode: IntegrationMode
  account: string
  providerExpiresAt: string | null
  status: 'ok' | 'expired'
  lastError: string
  wasInVm: boolean
  canEdit: boolean
  canDelete: boolean
}

export interface IntegrationScope extends WritableScope {
  connected: IntegrationKind[]
}

export const kindLabels: Record<IntegrationKind, string> = { github: 'GitHub', claude: 'Claude', linear: 'Linear' }
export const defaultSource: Record<IntegrationKind, string> = { github: 'pat', claude: 'setup-token', linear: 'api-key' }

export const tokenPages: Record<IntegrationKind, string> = {
  github: 'https://github.com/settings/tokens',
  claude: 'https://claude.ai/settings/claude-code',
  linear: 'https://linear.app/settings/account/security',
}
