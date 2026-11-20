import assert from 'node:assert/strict'
import { test } from 'node:test'

import { formatValue, logParams, prettyLine, rangeFor, type LogLine } from './telemetry.ts'

test('rangeFor spans the preset ending at now', () => {
  const now = new Date('2026-11-20T12:00:00Z')
  const r = rangeFor('6h', now)
  assert.equal(r.to.getTime(), now.getTime())
  assert.equal(now.getTime() - r.from.getTime(), 6 * 3600e3)
})

test('formatValue', () => {
  assert.equal(formatValue(1536, 'By'), '1.5 KB')
  assert.equal(formatValue(42.04, '%'), '42.0 %')
  assert.equal(formatValue(3 * 1024 ** 3, 'By'), '3.0 GB')
  assert.equal(formatValue(2048, 'By/s'), '2.0 KB/s')
})

test('logParams repeats field and skips empty filters', () => {
  const p = logParams({
    from: new Date('2026-11-20T10:00:00Z'),
    to: new Date('2026-11-20T11:00:00Z'),
    q: 'pull',
    level: '',
    source: 'firecracker',
    fields: { vm: 'vm01', disk: 'data-01' },
  })
  assert.deepEqual(p.getAll('field'), ['vm:vm01', 'disk:data-01'])
  assert.equal(p.get('from'), '2026-11-20T10:00:00.000Z')
  assert.equal(p.get('q'), 'pull')
  assert.equal(p.get('source'), 'firecracker')
  assert.equal(p.has('level'), false)
})

test('prettyLine makes only non-observatory fields clickable', () => {
  const l: LogLine = {
    time: '2026-11-20T10:00:00Z',
    level: 'info',
    source: 'observatory-node',
    msg: 'vm started',
    fields: { vm: 'vm01', 'observatory.node': 'n1' },
  }
  const lines = prettyLine(l)
  assert.equal(lines[0]?.text, '{')
  assert.equal(lines.at(-1)?.text, '}')
  const vm = lines.find((x) => x.key === 'vm')
  assert.equal(vm?.clickable, true)
  assert.equal(vm?.text, '  "vm": "vm01",')
  assert.equal(lines.find((x) => x.key === 'observatory.node')?.clickable, false)
  assert.equal(lines.find((x) => x.key === 'msg')?.clickable, false)
  assert.equal(lines.at(-2)?.text.endsWith(','), false)
})
