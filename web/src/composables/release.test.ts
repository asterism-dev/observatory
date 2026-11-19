import assert from 'node:assert/strict'
import { test } from 'node:test'

import { showBanner } from './release.ts'

const me = (platformAdmin: boolean, latestVersion: string) => ({ user: { platformAdmin }, latestVersion })

test('banner shown to platform admin when a newer release exists', () => {
  assert.equal(showBanner(me(true, 'v0.4.0'), null), true)
})

test('banner hidden from non-admins', () => {
  assert.equal(showBanner(me(false, 'v0.4.0'), null), false)
})

test('banner hidden without a newer release', () => {
  assert.equal(showBanner(me(true, ''), null), false)
})

test('banner hidden once this version was dismissed', () => {
  assert.equal(showBanner(me(true, 'v0.4.0'), 'v0.4.0'), false)
})

test('dismissing an older release does not hide a newer one', () => {
  assert.equal(showBanner(me(true, 'v0.4.0'), 'v0.3.9'), true)
})

test('banner hidden before me loaded', () => {
  assert.equal(showBanner(null, null), false)
})
