import assert from 'node:assert/strict'
import { test } from 'node:test'

import { chartUnits, copyPanel, dashboardScopes, legendLabels, limiter, metricMatch, newPanel, panelError, panelKey, stepFor, toSeries, wordAt, type Panel } from './dashboards.ts'
import { formatValue } from './telemetry.ts'

test('formatValue covers the panel units', () => {
  assert.equal(formatValue(0.0123, chartUnits.none), '0.0123')
  assert.equal(formatValue(1234567, chartUnits.none), '1.23 M')
  assert.equal(formatValue(-4200, chartUnits.none), '-4.2 k')
  assert.equal(formatValue(12.345, chartUnits.percent), '12.3 %')
  assert.equal(formatValue(2048, chartUnits.bytes_per_second), '2.0 KB/s')
  assert.equal(formatValue(0.0042, chartUnits.seconds), '4.2 ms')
  assert.equal(formatValue(0.0000042, chartUnits.seconds), '4.2 µs')
  assert.equal(formatValue(2.5, chartUnits.seconds), '2.5 s')
  assert.equal(formatValue(5400, chartUnits.seconds), '1.5 h')
  assert.equal(formatValue(12.5, chartUnits.requests_per_second), '12.5 req/s')
  assert.equal(formatValue(0, chartUnits.seconds), '0 s')
})

test('legendLabels prefers the VM name, hides internal labels and stays unique', () => {
  const labels = legendLabels([
    { __name__: 'orders_total', observatory_org: 'o', observatory_vm: 'v1', observatory_vm_name: 'vm01', status: '200' },
    { observatory_node: 'n1' },
    { observatory_node: 'n2' },
    { observatory_vm_name: 'vm02' },
    { b: '2', a: '1' },
  ])
  assert.deepEqual(labels, ['vm01 status=200', 'value', 'value #2', 'vm02', 'a=1 b=2'])
  assert.equal(legendLabels([{ __name__: 'up' }])[0], 'up')
  assert.equal(legendLabels([{ path: 'x'.repeat(100) }])[0]?.length, 60)
})

test('stepFor picks a round step for about 300 points', () => {
  const from = new Date('2026-11-20T00:00:00Z')
  const after = (h: number) => new Date(from.getTime() + h * 3600e3)
  assert.equal(stepFor(from, after(1)), 15)
  assert.equal(stepFor(from, after(6)), 120)
  assert.equal(stepFor(from, after(24)), 300)
  assert.equal(stepFor(from, after(168)), 3600)
  assert.equal(stepFor(from, after(24 * 31)), 10800)
})

test('metricMatch escapes the prefix for regex and string literal', () => {
  assert.equal(metricMatch('orders'), '{__name__=~"orders.*"}')
  assert.equal(metricMatch('a.b'), '{__name__=~"a\\\\.b.*"}')
  assert.equal(metricMatch('x"|y'), '{__name__=~"x\\"\\\\|y.*"}')
})

test('wordAt returns the metric-name token before the cursor', () => {
  const q = 'sum(rate(orders_to[5m]))'
  assert.deepEqual(wordAt(q, q.indexOf('[')), { start: 9, word: 'orders_to' })
  assert.deepEqual(wordAt('rate(', 5), { start: 5, word: '' })
})

test('panelError mirrors the API validation', () => {
  const ok: Panel = { title: 'CPU', type: 'timeseries', query: 'up', unit: 'none', width: 'half' }
  assert.equal(panelError(ok), '')
  assert.equal(panelError({ ...ok, query: '  ' }), 'query')
  assert.equal(panelError({ ...ok, query: 'ä'.repeat(2049) }), 'query')
  assert.equal(panelError({ ...ok, title: 'x'.repeat(101) }), 'title')
  assert.equal(panelError({ ...ok, title: 'a\u0007' }), 'title')
  assert.equal(panelError({ ...ok, unit: 'kg' as Panel['unit'] }), 'unit')
})

test('toSeries names series and keeps the time axis', () => {
  const s = toSeries({ t: [1, 2], series: [{ labels: { observatory_vm_name: 'vm01' }, values: [1, null] }] })
  assert.deepEqual(s, [{ name: 'vm01', values: [1, null] }])
})

test('dashboardScopes offers private only to the creator and org only with rights', () => {
  const teams = [{ id: 't1', name: 'core' }]
  assert.deepEqual(dashboardScopes(teams, false, true), [{ scope: 'user' }, { scope: 'team', teamId: 't1', teamName: 'core' }])
  assert.deepEqual(dashboardScopes([], true, false), [{ scope: 'org' }])
})

test('limiter runs at most max calls at once and all of them', async () => {
  const run = limiter(2)
  let active = 0
  let peak = 0
  const task = (v: number) =>
    run(async () => {
      peak = Math.max(peak, ++active)
      await new Promise((r) => setTimeout(r, 5))
      active--
      if (v === 3) throw new Error('boom')
      return v
    })
  const results = await Promise.allSettled([1, 2, 3, 4, 5].map(task))
  assert.equal(peak, 2)
  assert.deepEqual(
    results.map((r) => (r.status === 'fulfilled' ? r.value : 'failed')),
    [1, 2, 'failed', 4, 5],
  )
})

test('panelKey is stable per panel object and kept by copyPanel', () => {
  const a = newPanel('up')
  const b = { ...a }
  assert.equal(panelKey(a), panelKey(a))
  assert.notEqual(panelKey(a), panelKey(b))
  const c = copyPanel(a)
  assert.notEqual(c, a)
  assert.equal(panelKey(c), panelKey(a))
})
