import assert from 'node:assert/strict'
import { test } from 'node:test'

import { filterGroups, groupName, isStats, queryBody, type LogGroup } from './logs.ts'

const g = (kind: LogGroup['kind'], name: string, deleted = false): LogGroup => ({ kind, id: `${kind}-${name}`, name, deleted, streams: 1, lastAt: '2026-11-21T10:00:00Z' })

test('group display names per kind', () => {
  assert.equal(groupName(g('vm', 'web')), 'vms/web')
  assert.equal(groupName(g('node', 'n1')), 'nodes/n1')
  assert.equal(groupName(g('image', 'base')), 'builds/base')
})

test('filterGroups by kind and name keeps deleted groups', () => {
  const all = [g('vm', 'web'), g('vm', 'old', true), g('node', 'web')]
  assert.deepEqual(filterGroups(all, 'vm', '').map((x) => x.name), ['web', 'old'])
  assert.deepEqual(filterGroups(all, 'all', ' NODES/').map((x) => x.kind), ['node'])
  assert.deepEqual(filterGroups(all, 'all', 'old').filter((x) => x.deleted).length, 1)
})

test('queryBody sends ids only and defaults to match-all', () => {
  const b = queryBody('  ', [{ ...g('vm', 'web') }], { from: new Date('2026-11-21T09:00:00Z'), to: new Date('2026-11-21T10:00:00Z') })
  assert.deepEqual(b, { query: '*', groups: [{ kind: 'vm', id: 'vm-web' }], from: '2026-11-21T09:00:00.000Z', to: '2026-11-21T10:00:00.000Z', limit: 1000 })
})

test('isStats detects aggregated rows', () => {
  assert.equal(isStats([{ _time: 't', _msg: 'x' }]), false)
  assert.equal(isStats([{ 'observatory.vm_name': 'a', 'count(*)': '3' }]), true)
})
