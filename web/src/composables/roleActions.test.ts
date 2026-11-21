import assert from 'node:assert/strict'
import { test } from 'node:test'
import { actionSuggestions, unknownEffects } from './roleActions.ts'

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
