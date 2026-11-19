import assert from 'node:assert/strict'
import { test } from 'node:test'

import { appendCapped, buildDuration, defaultSourceLabel, refLabel } from './images.ts'

const teams = { t1: 'backend' }

test('refLabel qualifies org, private and team references like the CLI', () => {
  assert.equal(refLabel({ kind: 'secret', scope: 'org', name: 'NPM_TOKEN' }, teams), 'secret:org/NPM_TOKEN')
  assert.equal(refLabel({ kind: 'integration', scope: 'user', name: 'github' }, teams), 'integration:private/github')
  assert.equal(refLabel({ kind: 'secret', scope: 'team', teamId: 't1', name: 'KEY' }, teams), 'secret:team:backend/KEY')
})

test('refLabel falls back to the team id for unknown teams', () => {
  assert.equal(refLabel({ kind: 'secret', scope: 'team', teamId: 't9', name: 'KEY' }, teams), 'secret:team:t9/KEY')
})

const build = (startedAt: string | null, finishedAt: string | null) => ({ startedAt, finishedAt })

test('buildDuration is empty before the build starts', () => {
  assert.equal(buildDuration(build(null, null)), '')
})

test('buildDuration formats finished builds', () => {
  assert.equal(buildDuration(build('2026-11-19T10:00:00Z', '2026-11-19T10:00:42Z')), '42s')
  assert.equal(buildDuration(build('2026-11-19T10:00:00Z', '2026-11-19T10:03:05Z')), '3m 5s')
  assert.equal(buildDuration(build('2026-11-19T10:00:00Z', '2026-11-19T11:02:00Z')), '1h 2m')
})

test('buildDuration counts running builds up to now', () => {
  assert.equal(buildDuration(build('2026-11-19T10:00:00Z', null), Date.parse('2026-11-19T10:01:00Z')), '1m 0s')
})

test('defaultSourceLabel translates each cascade source', () => {
  const t = (k: string) => `<${k}>`
  for (const s of ['explicit', 'user-default', 'team-default', 'org-default', 'platform']) {
    assert.equal(defaultSourceLabel(s, t), `<images.source.${s}>`)
  }
})

test('appendCapped appends below the cap', () => {
  assert.deepEqual(appendCapped({ text: 'a\n', truncated: false }, 'b\n', 10), { text: 'a\nb\n', truncated: false })
})

test('appendCapped keeps whole trailing lines within the cap and marks truncation', () => {
  const got = appendCapped({ text: 'line1\nline2\n', truncated: false }, 'line3\n', 10)
  assert.deepEqual(got, { text: 'line3\n', truncated: true })
  assert.equal(appendCapped(got, 'x\n', 10).truncated, true)
})

test('appendCapped keeps the raw tail when it has no line break', () => {
  assert.deepEqual(appendCapped({ text: '', truncated: false }, 'abcdefghijkl', 5), { text: 'hijkl', truncated: true })
})
