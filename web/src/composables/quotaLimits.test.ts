import assert from 'node:assert/strict'
import { test } from 'node:test'

import type { Limits as QuotaLimits } from '../gen/observatory/quota/v1/quota_pb.ts'
import { fromQuota, toQuota } from './quotaLimits.ts'

test('fromQuota reads an absent value or scope as unlimited', () => {
  assert.deepEqual(fromQuota({ vcpu: 4, vmCount: 0 } as QuotaLimits), { vcpu: 4, ramMb: null, diskGb: null, vmCount: 0, snapshotCount: null, imageStorageGb: null })
  assert.deepEqual(fromQuota(undefined), { vcpu: null, ramMb: null, diskGb: null, vmCount: null, snapshotCount: null, imageStorageGb: null })
})

test('toQuota leaves unlimited values out', () => {
  assert.deepEqual(toQuota({ vcpu: 2, ramMb: null, diskGb: 0, vmCount: null, snapshotCount: null, imageStorageGb: 5 }), { vcpu: 2, diskGb: 0, imageStorageGb: 5 })
})
