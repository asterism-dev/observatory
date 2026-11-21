import assert from 'node:assert/strict'
import { test } from 'node:test'

import de from '../locales/de.json' with { type: 'json' }
import en from '../locales/en.json' with { type: 'json' }
import { deletionLabel, imageLine, lifetime, liveUsage, type NodeInfo, type VmInfo } from './nodes.ts'

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

test('lifetime shows the two largest units', () => {
  const at = (min: number, sec = 0) => new Date(Date.UTC(2026, 10, 20, 0, min, sec)).toISOString()
  assert.equal(lifetime(at(0), at(24 * 60)), '1d 0h')
  assert.equal(lifetime(at(0), at(157)), '2h 37m')
  assert.equal(lifetime(at(0), at(12)), '12m')
  assert.equal(lifetime(at(0), at(0, 30)), '0m')
})

test('deletion label says deleting once the time has passed', () => {
  const now = Date.UTC(2026, 10, 20, 12, 0)
  assert.equal(deletionLabel(new Date(now - 120000).toISOString(), translator(en), 'en', now), 'Deleting…')
  assert.equal(deletionLabel(new Date(now - 120000).toISOString(), translator(de), 'de', now), 'Wird gelöscht…')
  assert.equal(deletionLabel(new Date(now + 2 * 3600000).toISOString(), translator(en), 'en', now), 'deleted in 2 hours')
})

test('liveUsage turns fresh usage into percentages and drops stale usage', () => {
  const now = Date.UTC(2026, 10, 20, 12, 0)
  const usage = { at: new Date(now - 10000).toISOString(), cpuPercent: 42, memUsedMB: 512, memTotalMB: 2048, diskUsedMB: 2560, diskTotalMB: 10240 }
  assert.deepEqual(liveUsage({ usage } as VmInfo, now), { cpu: 42, mem: 25, dsk: 25 })
  assert.deepEqual(liveUsage({ usage: { ...usage, memTotalMB: 0, diskTotalMB: 0 } } as VmInfo, now), { cpu: 42, mem: null, dsk: null })
  assert.deepEqual(liveUsage({ usage: { ...usage, at: new Date(now - 60000).toISOString() } } as VmInfo, now), { cpu: null, mem: null, dsk: null })
  assert.deepEqual(liveUsage({} as VmInfo, now), { cpu: null, mem: null, dsk: null })
})
