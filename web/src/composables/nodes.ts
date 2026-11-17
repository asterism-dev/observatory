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
}

export const stateTones = { online: 'success', offline: 'danger', pending: 'warning' } as const

export const gb = (mb: number) => (mb / 1024).toFixed(1)
