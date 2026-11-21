import assert from 'node:assert/strict'
import { test } from 'node:test'

import type { Event } from '../gen/observatory/events/v1/event_pb.ts'
import { toAuditRow } from './useAudit.ts'

const event = (over: Partial<Event>): Event =>
  ({
    id: '0190f5a2-7c3b-7d4e-8f60-123456789abc',
    time: { seconds: 1763560680n, nanos: 500_000_000 },
    type: 'iam.members.delete',
    org: '0b9d6a43-5d43-4a43-9a43-1d43c5d43a43',
    actor: { kind: 1, id: 'u1' },
    resource: 'org/members/u2',
    result: 2,
    clientIp: '203.0.113.7',
    userAgent: 'ua',
    requestId: '',
    audit: true,
    ...over,
  }) as Event

test('toAuditRow maps the envelope to a table row', () => {
  assert.deepEqual(toAuditRow(event({})), {
    id: '0190f5a2-7c3b-7d4e-8f60-123456789abc',
    at: new Date(1763560680500),
    actorKind: 'user',
    actorId: 'u1',
    org: '0b9d6a43-5d43-4a43-9a43-1d43c5d43a43',
    action: 'iam.members.delete',
    resource: 'org/members/u2',
    result: 'denied',
  })
})

test('toAuditRow handles platform events and missing fields', () => {
  const row = toAuditRow(event({ org: '', actor: undefined, time: undefined, result: 3 }))
  assert.equal(row.org, '')
  assert.equal(row.actorKind, '')
  assert.equal(row.actorId, '')
  assert.equal(row.at.getTime(), 0)
  assert.equal(row.result, 'failed')
})

test('toAuditRow names every actor kind and result', () => {
  assert.deepEqual([2, 3, 4].map((kind) => toAuditRow(event({ actor: { kind, id: '' } as Event['actor'] })).actorKind), ['service', 'node', 'system'])
  assert.equal(toAuditRow(event({ result: 1 })).result, 'allowed')
})
