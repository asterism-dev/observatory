import assert from 'node:assert/strict'
import { test } from 'node:test'
import { actionSuggestions } from './roleActions.ts'

test('actionSuggestions offers service and resource wildcards without duplicates', () => {
  const got = actionSuggestions(['iam.roles.list', 'iam.roles.manage', 'iam.members.list', 'compute.nodes.get'])
  assert.deepEqual(got, [
    '*', 'iam.*', 'iam.roles.*', 'iam.members.*', 'compute.*', 'compute.nodes.*',
    'iam.roles.list', 'iam.roles.manage', 'iam.members.list', 'compute.nodes.get',
  ])
  assert.ok(got.every((a) => !a.includes(':')))
})
