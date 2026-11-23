import assert from 'node:assert/strict'
import { test } from 'node:test'

import { appendCapped, autoSettingsBody, buildDuration, commitOf, defaultSourceLabel, formatSource, refLabel } from './images.ts'

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

test('autoSettingsBody approves the shown revision only when turning automation on', () => {
  const off = { autoSchedule: '' as const, autoOnBaseChange: false, recipeRevision: 4 }
  assert.deepEqual(autoSettingsBody(off, 'weekly', false), { autoSchedule: 'weekly', autoOnBaseChange: false, approveRevision: 4 })
  assert.deepEqual(autoSettingsBody(off, 'off', true), { autoSchedule: '', autoOnBaseChange: true, approveRevision: 4 })
  assert.deepEqual(autoSettingsBody(off, 'off', false), { autoSchedule: '', autoOnBaseChange: false })
  const on = { autoSchedule: 'daily' as const, autoOnBaseChange: false, recipeRevision: 5 }
  assert.deepEqual(autoSettingsBody(on, 'weekly', true), { autoSchedule: 'weekly', autoOnBaseChange: true })
  assert.deepEqual(autoSettingsBody({ ...off, recipeRevision: null }, 'daily', false), { autoSchedule: 'daily', autoOnBaseChange: false })
})

test('formatSource renders repo, ref and path compactly', () => {
  assert.equal(formatSource({ repo: 'o/r', ref: '', path: '' }), 'o/r')
  assert.equal(formatSource({ repo: 'o/r', ref: 'main', path: 'img' }), 'o/r@main:img')
  assert.equal(formatSource(null), '')
})

test('commitOf parses a build source and ignores empty ones', () => {
  const sha = 'a'.repeat(40)
  assert.deepEqual(commitOf(`github:o/r@${sha}:img`), { repo: 'o/r', sha })
  assert.equal(commitOf(''), null)
})
