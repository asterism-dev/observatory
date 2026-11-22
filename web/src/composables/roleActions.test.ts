import assert from 'node:assert/strict'
import { test } from 'node:test'
import { actionHints, actionSuggestions, unknownEffects, unknownMarks } from './roleActions.ts'

test('actionSuggestions offers service and resource wildcards without duplicates', () => {
  const got = actionSuggestions(['iam.roles.list', 'iam.roles.manage', 'iam.members.list', 'compute.nodes.get'])
  assert.deepEqual(got, [
    '*', 'iam.*', 'iam.roles.*', 'iam.members.*', 'compute.*', 'compute.nodes.*',
    'iam.roles.list', 'iam.roles.manage', 'iam.members.list', 'compute.nodes.get',
  ])
  assert.ok(got.every((a) => !a.includes(':')))
})

test('unknownEffects lists effects the editor cannot represent, once each', () => {
  assert.deepEqual(unknownEffects([{ effect: 'allow' }, { effect: 'deny' }]), [])
  assert.deepEqual(unknownEffects([{ effect: 'audit' }, { effect: 'allow' }, { effect: 'audit' }, { effect: '' }]), ['audit', ''])
})

test('unknownMarks marks the rules whose action no service offers any more', () => {
  const rules = [{ action: 'compute.instances.fork' }, { action: 'compute.instances.get' }, { action: 'compute.instances.fork' }]
  assert.deepEqual(unknownMarks(rules, ['compute.instances.fork']), [true, false, true])
  assert.deepEqual(unknownMarks(rules, []), [false, false, false])
})

test('actionHints keeps only actions with a description', () => {
  const got = actionHints([
    { action: 'audit.settings.update', description: 'Change audit settings' },
    { action: 'compute.instances.get', description: '' },
  ])
  assert.deepEqual([...got], [['audit.settings.update', 'Change audit settings']])
})
