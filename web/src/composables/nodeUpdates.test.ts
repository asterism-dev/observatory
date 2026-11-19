import assert from 'node:assert/strict'
import { test } from 'node:test'

import { bannerRun, runSummary, selectable, vmImpact } from './nodeUpdates.ts'

const node = (status: string, name = 'n') => ({ id: name, name, status, reason: '' })
const run = (nodes: ReturnType<typeof node>[]) => ({ id: 'r', state: 'running', startedBy: 'u', startedAt: '', endedAt: null, nodes }) as never

test('runSummary counts done nodes and names the updating one', () => {
  assert.deepEqual(runSummary(run([node('done', 'a'), node('updating', 'b'), node('pending', 'c')])), { done: 1, total: 3, current: 'b' })
})

test('runSummary has no current node when none is updating', () => {
  assert.equal(runSummary(run([node('done'), node('failed')])).current, null)
})

test('selectable requires updatable and no active run', () => {
  assert.equal(selectable({ updatable: true } as never, false), true)
  assert.equal(selectable({ updatable: true } as never, true), false)
  assert.equal(selectable({ updatable: false } as never, false), false)
})

test('vmImpact follows updatePausesOnly', () => {
  assert.equal(vmImpact({ updatePausesOnly: true } as never), 'pause')
  assert.equal(vmImpact({ updatePausesOnly: false } as never), 'restart')
})

const withState = (state: string, id = 'r') => ({ ...run([]), id, state }) as never

test('bannerRun hides a missing run', () => {
  assert.equal(bannerRun(null, new Set()), null)
})

test('bannerRun always shows running and failed runs', () => {
  assert.notEqual(bannerRun(withState('running'), new Set()), null)
  assert.notEqual(bannerRun(withState('failed'), new Set()), null)
})

test('bannerRun hides done and cancelled runs that were not seen running', () => {
  assert.equal(bannerRun(withState('done'), new Set()), null)
  assert.equal(bannerRun(withState('cancelled'), new Set(['other'])), null)
})

test('bannerRun shows done and cancelled runs that were seen running', () => {
  const seen = new Set(['r'])
  assert.notEqual(bannerRun(withState('done'), seen), null)
  assert.notEqual(bannerRun(withState('cancelled'), seen), null)
})
