import assert from 'node:assert/strict'
import { test } from 'node:test'

import type { GetMeResponse } from '../gen/observatory/iam/v1/iam_pb.ts'
import { toMe } from './me.ts'

const info = { docsURL: 'https://docs.example', version: '1.0.0', latestVersion: '1.1.0' }

test('toMe merges GetMe and info into the me shape', () => {
  const res = {
    user: { id: 'u1', email: 'a@example.com', name: 'A', platformAdmin: true },
    orgs: [{ id: 'o1', name: 'Personal', personal: true, role: 'owner' }],
  } as GetMeResponse
  assert.deepEqual(toMe(res, info), {
    user: { id: 'u1', email: 'a@example.com', name: 'A', platformAdmin: true },
    orgs: [{ id: 'o1', name: 'Personal', personal: true, role: 'owner' }],
    ...info,
  })
})

test('toMe tolerates a missing user and no orgs', () => {
  const me = toMe({ orgs: [] } as unknown as GetMeResponse, info)
  assert.deepEqual(me.user, { id: '', email: '', name: '', platformAdmin: false })
  assert.deepEqual(me.orgs, [])
})
