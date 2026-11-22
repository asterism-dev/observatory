import type { Limits } from '../components/LimitFields.vue'
import type { Limits as QuotaLimits } from '../gen/observatory/quota/v1/quota_pb.ts'

const keys = ['vcpu', 'ramMb', 'diskGb', 'vmCount', 'snapshotCount', 'imageStorageGb'] as const

// Quota leaves unlimited values (and scopes without limits) out; the editor shows them as null.
export const fromQuota = (l: QuotaLimits | undefined): Limits => Object.fromEntries(keys.map((k) => [k, l?.[k] ?? null])) as unknown as Limits

export const toQuota = (l: Limits): Partial<Record<(typeof keys)[number], number>> =>
  Object.fromEntries(keys.filter((k) => l[k] !== null).map((k) => [k, l[k]]))
