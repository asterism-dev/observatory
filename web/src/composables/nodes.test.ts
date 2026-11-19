import assert from 'node:assert/strict'
import { test } from 'node:test'

import de from '../locales/de.json' with { type: 'json' }
import en from '../locales/en.json' with { type: 'json' }
import { imageLine, type NodeInfo } from './nodes.ts'

type Image = NonNullable<NodeInfo['image']>
const translator = (messages: typeof en) => (key: string, params: Record<string, unknown> = {}) => {
  const msg = key.split('.').reduce<any>((m, k) => m?.[k], messages) as string
  return msg.replace(/\{(\w+)\}/g, (_, k) => String(params[k]))
}
const ref = 'ghcr.io/x/guest:1.0'

test('pulling image line', () => {
  const img: Image = { state: 'pulling', version: ref }
  assert.equal(imageLine(img, translator(en), 'en'), `${ref} · Pulling image`)
  assert.equal(imageLine(img, translator(de), 'de'), `${ref} · Image wird geladen`)
})

test('ready image line without build time', () => {
  const img: Image = { state: 'ready', version: ref }
  assert.equal(imageLine(img, translator(en), 'en'), `${ref} · ready`)
  assert.equal(imageLine(img, translator(de), 'de'), `${ref} · bereit`)
})

test('failed image line with error and retry', () => {
  const t = translator(en)
  assert.equal(imageLine({ state: 'failed', version: ref, error: 'disk full' }, t, 'en'), `${ref} · failed: disk full`)
  assert.match(imageLine({ state: 'failed', version: ref, error: 'x', retryAt: new Date(Date.now() + 3600e3).toISOString() }, t, 'en'), /next try/)
  assert.equal(imageLine(null, t, 'en'), '–')
})
