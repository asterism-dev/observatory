import assert from 'node:assert/strict'
import { test } from 'node:test'

import { Code, ConnectError } from '@connectrpc/connect'

import { errorKind, errorText } from './apiError.ts'

class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

test('errorKind classifies REST and Connect errors alike', () => {
  const cases: [unknown, string][] = [
    [new ApiError(401, 'x'), 'unauthenticated'],
    [new ConnectError('x', Code.Unauthenticated), 'unauthenticated'],
    [new ApiError(403, 'x'), 'forbidden'],
    [new ConnectError('x', Code.PermissionDenied), 'forbidden'],
    [new ApiError(404, 'x'), 'notFound'],
    [new ConnectError('x', Code.NotFound), 'notFound'],
    [new ApiError(500, 'x'), 'other'],
    [new ConnectError('x', Code.Internal), 'other'],
    [new Error('x'), 'other'],
  ]
  for (const [e, kind] of cases) assert.equal(errorKind(e), kind)
})

test('errorText drops the Connect code prefix', () => {
  assert.equal(errorText(new ConnectError('use LeaveOrg', Code.InvalidArgument)), 'use LeaveOrg')
  assert.equal(errorText(new ApiError(400, 'bad')), 'bad')
  assert.equal(errorText('boom'), 'boom')
})
