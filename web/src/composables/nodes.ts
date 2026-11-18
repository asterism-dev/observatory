export interface NodeInfo {
  id: string
  name: string
  state: 'pending' | 'online' | 'offline'
  version: string
  cpus: number
  memoryMB: number
  machine: Record<string, unknown> | null
  lastSeen: string | null
  vmCount: number
  usedCPUs: number
  usedMemoryMB: number
  canUpdate: boolean
  canDelete: boolean
  underlay: { ip: string; registered: boolean; lastHandshake: string | null } | null
  quarantined: boolean
}

export const stateTones = { online: 'success', offline: 'danger', pending: 'warning' } as const

export const gb = (mb: number) => (mb / 1024).toFixed(1)

export interface VmInfo {
  id: string
  name: string
  owner: { kind: 'user' | 'team'; id: string; name: string }
  node: { id: string; name: string }
  state: 'pending' | 'starting' | 'running' | 'stopped' | 'paused' | 'hibernated' | 'suspended' | 'error' | 'unknown'
  power: 'running' | 'stopped' | 'paused' | 'hibernated'
  error?: string
  ip?: string
  mgmtIP?: string
  dnsName: string
  networks: { id: string; name: string; ip: string; dnsName: string }[]
  canNetwork: boolean
  disks: { id: string; name: string; mountPath: string; sizeGB: number; state: string; error?: string }[]
  canDisks: boolean
  cpus: number
  memoryMB: number
  diskGB: number
  createdAt: string
  canPower: boolean
  canDelete: boolean
}

export const vmTones = {
  running: 'success',
  starting: 'warning',
  pending: 'warning',
  stopped: 'muted',
  paused: 'warning',
  hibernated: 'muted',
  suspended: 'muted',
  error: 'danger',
  unknown: 'muted',
} as const

// A restart while the VM's memory is saved boots it fresh and discards that memory.
export const keepsMemory = (vm: VmInfo) => vm.power === 'paused' || vm.power === 'hibernated' || vm.state === 'suspended'

export const powerActions = (state: VmInfo['state']) => {
  if (state === 'running') return ['pause', 'hibernate', 'shutdown', 'stop']
  if (state === 'paused' || state === 'hibernated' || state === 'suspended') return ['resume', 'stop']
  return ['start']
}
