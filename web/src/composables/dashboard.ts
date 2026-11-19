// No runtime imports: node --test runs this file directly.

export interface DashboardInput {
  me: string
  canCreateNode: boolean
  // null: the list answered 403.
  nodes: { state: 'pending' | 'online' | 'offline' }[] | null
  sshKeys: number
  devices: { owner: { id: string }; lastHandshake: string | null }[] | null
  vms: { name: string; state: string }[] | null
  disks: { sizeGB: number; attachment?: unknown }[] | null
  now: number
}

export type StepId = 'node' | 'sshKey' | 'device' | 'vm' | 'ssh'

export interface GuideStep {
  id: StepId
  done: boolean
}

// WireGuard re-handshakes every 2 minutes while the tunnel carries traffic or keepalives.
export const CONNECTED_MS = 180_000

const connected = (d: { lastHandshake: string | null }, now: number) =>
  !!d.lastHandshake && now - Date.parse(d.lastHandshake) <= CONNECTED_MS

export function sshTarget(i: DashboardInput): string | null {
  return i.vms?.find((v) => v.state === 'running')?.name ?? null
}

export function guideSteps(i: DashboardInput): GuideStep[] {
  const mine = (i.devices ?? []).filter((d) => d.owner.id === i.me)
  const steps: GuideStep[] = []
  // Members only see the node step as the blocker it is: they cannot add one.
  if (i.nodes && (i.canCreateNode || i.nodes.length === 0)) steps.push({ id: 'node', done: i.nodes.length > 0 })
  steps.push(
    { id: 'sshKey', done: i.sshKeys > 0 },
    { id: 'device', done: mine.length > 0 },
    { id: 'vm', done: sshTarget(i) !== null },
    { id: 'ssh', done: sshTarget(i) !== null && mine.some((d) => !!d.lastHandshake) },
  )
  return steps
}

export function tiles(i: DashboardInput) {
  const out: {
    vms?: { running: number; stopped: number; other: number }
    nodes?: { online: number; offline: number }
    disks?: { count: number; totalGB: number; attached: number }
    devices?: { connected: number; total: number }
  } = {}
  if (i.vms) {
    const running = i.vms.filter((v) => v.state === 'running').length
    const stopped = i.vms.filter((v) => v.state === 'stopped').length
    out.vms = { running, stopped, other: i.vms.length - running - stopped }
  }
  if (i.nodes) {
    const online = i.nodes.filter((n) => n.state === 'online').length
    out.nodes = { online, offline: i.nodes.length - online }
  }
  if (i.disks) {
    out.disks = {
      count: i.disks.length,
      totalGB: i.disks.reduce((sum, d) => sum + d.sizeGB, 0),
      attached: i.disks.filter((d) => d.attachment).length,
    }
  }
  if (i.devices) out.devices = { connected: i.devices.filter((d) => connected(d, i.now)).length, total: i.devices.length }
  return out
}
