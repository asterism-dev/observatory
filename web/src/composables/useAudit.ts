import type { Event } from '../gen/observatory/events/v1/event_pb.ts'

export interface AuditRow {
  id: string
  at: Date
  actorKind: string
  actorId: string
  org: string
  action: string
  resource: string
  result: 'allowed' | 'denied' | 'failed' | ''
}

// Indexed by the ActorKind and Result enum numbers; the generated enums are not importable from node tests.
const actorKinds = ['', 'user', 'service', 'node', 'system']
const results = ['', 'allowed', 'denied', 'failed'] as const

export function toAuditRow(e: Event): AuditRow {
  return {
    id: e.id,
    at: new Date(e.time ? Number(e.time.seconds) * 1000 + e.time.nanos / 1e6 : 0),
    actorKind: actorKinds[e.actor?.kind ?? 0] ?? '',
    actorId: e.actor?.id ?? '',
    org: e.org,
    action: e.type,
    resource: e.resource,
    result: results[e.result] ?? '',
  }
}
