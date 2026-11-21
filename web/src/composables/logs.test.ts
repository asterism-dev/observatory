import assert from 'node:assert/strict'
import { test } from 'node:test'

import { filterGroups, groupName, logRange, rangeOptions, retentionSpan, queryBody, type LogGroup } from './logs.ts'

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

test('retentionSpan picks days or hours', () => {
  assert.deepEqual(retentionSpan(14 * 86400), { unit: 'days', n: 14 })
  assert.deepEqual(retentionSpan(36 * 3600), { unit: 'hours', n: 36 })
  assert.deepEqual(retentionSpan(60), { unit: 'hours', n: 1 })
})

test('rangeOptions adds the retention only beyond 7 days', () => {
  assert.equal(rangeOptions(['1h', '7d'], 14 * 86400, '14 days').at(-1)?.label, '14 days')
  assert.equal(rangeOptions(['1h', '7d'], 3 * 86400, '3 days').length, 2)
})

const hours = (_: string, now: Date) => ({ from: new Date(now.getTime() - 3600e3), to: now })

test('logRange reaches back the whole retention', () => {
  const now = new Date('2026-11-21T10:00:00Z')
  assert.equal(logRange('retention', 14 * 86400, now, hours).from.toISOString(), '2026-11-07T10:00:00.000Z')
  assert.equal(logRange('1h', 14 * 86400, now, hours).from.toISOString(), '2026-11-21T09:00:00.000Z')
})
