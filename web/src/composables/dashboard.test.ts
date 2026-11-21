import assert from 'node:assert/strict'
import { test } from 'node:test'

import { CONNECTED_MS, guideSteps, sshTarget, tiles, type DashboardInput } from './dashboard.ts'

const now = Date.parse('2026-11-19T12:00:00Z')
const ago = (ms: number) => new Date(now - ms).toISOString()

function input(over: Partial<DashboardInput> = {}): DashboardInput {
  return { me: 'u1', canCreateNode: false, nodes: [], sshKeys: 0, devices: [], vms: [], disks: [], now, ...over }
}

const step = (i: DashboardInput, id: string) => guideSteps(i).find((s) => s.id === id)

test('node step omitted for non-admin when nodes exist', () => {
  assert.equal(step(input({ nodes: [{ state: 'online' }] }), 'node'), undefined)
})

test('node step shown open to non-admin when org has no node', () => {
  assert.deepEqual(step(input(), 'node'), { id: 'node', done: false })
})

test('node step for admins', () => {
  assert.deepEqual(step(input({ canCreateNode: true }), 'node'), { id: 'node', done: false })
  assert.deepEqual(step(input({ canCreateNode: true, nodes: [{ state: 'offline' }] }), 'node'), { id: 'node', done: true })
})

test('node step omitted when nodes is null', () => {
  assert.equal(step(input({ nodes: null, canCreateNode: true }), 'node'), undefined)
})

test('steps keep their order', () => {
  assert.deepEqual(
    guideSteps(input({ canCreateNode: true })).map((s) => s.id),
    ['node', 'sshKey', 'device', 'vm', 'ssh'],
  )
})

test('ssh key step done with a key', () => {
  assert.equal(step(input({ sshKeys: 1 }), 'sshKey')?.done, true)
  assert.equal(step(input(), 'sshKey')?.done, false)
})

test('device and ssh count only my devices', () => {
  const i = input({
    devices: [{ owner: { id: 'u2' }, lastHandshake: ago(1000) }],
    vms: [{ name: 'a', state: 'running' }],
  })
  assert.equal(step(i, 'device')?.done, false)
  assert.equal(step(i, 'ssh')?.done, false)
})

test('ssh needs a handshake ever and a running vm', () => {
  const vms = [{ name: 'a', state: 'running' }]
  // An idle tunnel must not bring the finished guide back.
  assert.equal(step(input({ vms, devices: [{ owner: { id: 'u1' }, lastHandshake: ago(200_000) }] }), 'ssh')?.done, true)
  assert.equal(step(input({ vms, devices: [{ owner: { id: 'u1' }, lastHandshake: null }] }), 'ssh')?.done, false)
  const fresh = input({ vms, devices: [{ owner: { id: 'u1' }, lastHandshake: ago(10_000) }] })
  assert.equal(step(fresh, 'device')?.done, true)
  assert.equal(step(fresh, 'vm')?.done, true)
  assert.equal(step(fresh, 'ssh')?.done, true)
  assert.equal(step(input({ devices: [{ owner: { id: 'u1' }, lastHandshake: ago(10_000) }] }), 'ssh')?.done, false)
})

test('vm step needs a running vm', () => {
  assert.equal(step(input({ vms: [{ name: 'a', state: 'stopped' }] }), 'vm')?.done, false)
  assert.equal(step(input({ vms: null }), 'vm')?.done, false)
})

test('tiles leave out lists that were null', () => {
  assert.deepEqual(tiles(input({ nodes: null, vms: null, disks: null, devices: null })), {})
})

test('tiles count states', () => {
  const t = tiles(
    input({
      vms: [
        { name: 'a', state: 'running' },
        { name: 'b', state: 'running' },
        { name: 'c', state: 'stopped' },
        { name: 'd', state: 'hibernated' },
      ],
      nodes: [{ state: 'online' }, { state: 'offline' }, { state: 'pending' }, { state: 'underlay_down' }],
      disks: [{ sizeGB: 10 }, { sizeGB: 20, attachment: {} }],
      devices: [
        { owner: { id: 'u2' }, lastHandshake: ago(CONNECTED_MS - 1000) },
        { owner: { id: 'u1' }, lastHandshake: ago(CONNECTED_MS + 1000) },
        { owner: { id: 'u1' }, lastHandshake: null },
      ],
    }),
  )
  assert.deepEqual(t.vms, { running: 2, stopped: 1, other: 1 })
  assert.deepEqual(t.nodes, { online: 1, offline: 3 })
  assert.deepEqual(t.disks, { count: 2, totalGB: 30, attached: 1 })
  assert.deepEqual(t.devices, { connected: 1, total: 3 })
})

test('sshTarget returns the first running vm', () => {
  assert.equal(sshTarget(input({ vms: [{ name: 'a', state: 'stopped' }, { name: 'b', state: 'running' }] })), 'b')
  assert.equal(sshTarget(input()), null)
  assert.equal(sshTarget(input({ vms: null })), null)
})
