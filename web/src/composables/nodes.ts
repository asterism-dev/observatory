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
  image: {
    state: 'building' | 'ready' | 'failed' | 'custom'
    version: string
    fallback?: string
    startedAt?: string
    builtAt?: string
    error?: string
    retryAt?: string
  } | null
}

export const stateTones = { online: 'success', offline: 'danger', pending: 'warning' } as const

export const gb = (mb: number) => (mb / 1024).toFixed(1)

// Samples arrive every 10 s while the VM runs; older ones describe a VM that stopped.
export function liveUsage(vm: VmInfo, now = Date.now()) {
  const u = vm.usage
  if (!u || now - Date.parse(u.at) > 30000) return { cpu: null, mem: null, dsk: null }
  return {
    cpu: u.cpuPercent,
    mem: u.memTotalMB ? (u.memUsedMB / u.memTotalMB) * 100 : null,
    dsk: vm.diskGB ? (u.diskUsedMB / (vm.diskGB * 1024)) * 100 : null,
  }
}

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
  canSnapshot: boolean
  canRestore: boolean
  canFork: boolean
  cpus: number
  memoryMB: number
  diskGB: number
  createdAt: string
  canPower: boolean
  autoSuspendMinutes: number
  canDelete: boolean
  image: string
  stockImage: boolean
  usage?: { at: string; cpuPercent: number; memTotalMB: number; memUsedMB: number; diskUsedMB: number }
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
  if (state === 'paused' || state === 'hibernated' || state === 'suspended') return ['resume', 'shutdown', 'stop']
  return ['start']
}

export function relativeTime(iso: string, locale: string) {
  const sec = Math.round((new Date(iso).getTime() - Date.now()) / 1000)
  const units: [Intl.RelativeTimeFormatUnit, number][] = [['day', 86400], ['hour', 3600], ['minute', 60]]
  const [unit, size] = units.find(([, s]) => Math.abs(sec) >= s) ?? ['second', 1]
  return new Intl.RelativeTimeFormat(locale, { numeric: 'auto' }).format(Math.round(sec / size), unit)
}

export function elapsed(iso: string, locale: string) {
  const min = Math.max(0, Math.floor((Date.now() - new Date(iso).getTime()) / 60000))
  const [unit, n] = min >= 1440 ? ['day', Math.floor(min / 1440)] : min >= 60 ? ['hour', Math.floor(min / 60)] : ['minute', min]
  return new Intl.NumberFormat(locale, { style: 'unit', unit, unitDisplay: 'short' }).format(n)
}

function fallback(img: NonNullable<NodeInfo['image']>, t: (k: string, p?: Record<string, unknown>) => string) {
  return img.fallback ? ` · ${t('nodes.image.fallback', { version: img.fallback })}` : ''
}

export function imageLine(img: NodeInfo['image'], t: (k: string, p?: Record<string, unknown>) => string, locale: string) {
  if (!img) return '–'
  const v = img.version
  switch (img.state) {
    case 'ready':
      return `${v} · ${img.builtAt ? t('nodes.image.readyAt', { time: relativeTime(img.builtAt, locale) }) : t('nodes.image.ready')}`
    case 'building':
      return `${v} · ${img.startedAt ? t('nodes.image.buildingFor', { time: elapsed(img.startedAt, locale) }) : t('nodes.image.building')}${fallback(img, t)}`
    case 'failed': {
      const error = img.error ?? ''
      return `${v} · ${img.retryAt ? t('nodes.image.failedRetry', { error, time: relativeTime(img.retryAt, locale) }) : t('nodes.image.failed', { error })}${fallback(img, t)}`
    }
    default: return ''
  }
}
