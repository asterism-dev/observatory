<script setup lang="ts">
import { Button, Combobox, SegmentedControl, Textarea, TextInput } from '@asterism-dev/lumen'
import { TriangleAlert } from '@lucide/vue'
import { computed, ref, watch, watchEffect } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'

import { errorMessage as message, iamClient } from '@/composables/iamClient'
import { actionHints, actionSuggestions, unknownEffects, unknownMarks } from '@/composables/roleActions'
import { useCrumbs } from '@/composables/useCrumbs'
import { useOrg } from '@/composables/useOrg'
import type { CatalogEntry as Perm, Role } from '@/gen/observatory/iam/v1/iam_pb'
import { Card, Section, Tag } from '@/ui'

interface Rule {
  uid: number
  // Kept as sent so an effect this editor does not know is never saved as 'allow'.
  effect: string
  object: string
  action: string
}

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const { org, can, reload } = useOrg()

const roles = ref<Role[]>([])
const catalog = ref<Perm[]>([])
const unknownActions = ref<string[]>([])
const name = ref('')
const description = ref('')
const parents = ref<string[]>([])
const rules = ref<Rule[]>([])
const addParent = ref('')
let nextUid = 0
const withUid = (r: Omit<Rule, 'uid'>): Rule => ({ ...r, uid: nextUid++ })
const searches = ref<Record<string, string>>({})
const busy = ref(false)
const error = ref('')

const roleId = computed(() => route.params.roleId as string | undefined)
const current = computed(() => roles.value.find((r) => (r.key || r.id) === roleId.value))
const readonly = computed(() => !!current.value?.builtin || !can('iam.roles.manage'))
const { setCrumbs } = useCrumbs()
watchEffect(() => {
  const label = roleId.value === 'new' ? t('roles.create') : current.value?.name
  if (label) setCrumbs(route.path, [{ label, to: route.path }])
})
const badEffects = computed(() => unknownEffects(rules.value))
const unknownRules = computed(() => unknownMarks(rules.value, unknownActions.value))
const canDeny = computed(() => ['owner', 'admin'].includes(org.value?.role ?? ''))

const effectOptions = computed(() => [
  { value: 'allow' as const, label: t('roles.allow') },
  { value: 'deny' as const, label: t('roles.deny'), disabled: !canDeny.value },
])
const parentLabel = (p: string) => roles.value.find((r) => (r.key || r.id) === p)?.name ?? p
const parentOptions = computed(() =>
  roles.value
    .filter((r) => (r.id ? r.id !== roleId.value : r.key !== 'owner'))
    .map((r) => ({ value: r.key || r.id, label: r.name }))
    .filter((o) => !parents.value.includes(o.value)),
)

const objects = computed(() => [...new Set(catalog.value.map((p) => p.object))])
const actions = computed(() => actionSuggestions(catalog.value.map((p) => p.action)))
const hints = computed(() => actionHints(catalog.value))

// Combobox filters are the caller's job; the typed text is offered as a pick so free-form values work.
function suggestions(pool: string[], key: string, hint?: Map<string, string>) {
  const q = (searches.value[key] ?? '').trim()
  const hits = pool.filter((v) => v.toLowerCase().includes(q.toLowerCase())).slice(0, 50)
  if (q && !pool.includes(q)) hits.unshift(q)
  return hits.map((v) => ({ value: v, label: v, title: hint?.get(v) }))
}

async function load() {
  if (!org.value) return
  const id = org.value.id
  try {
    const [rs, cat] = await Promise.all([
      iamClient.listRoles({ org: id }).then((r) => r.roles),
      iamClient.listCatalog({ org: id }),
    ])
    // Org switched while loading.
    if (id !== org.value?.id) return
    roles.value = rs
    catalog.value = cat.entries
    unknownActions.value = cat.unknownActions
    const r = rs.find((x) => (x.key || x.id) === roleId.value)
    name.value = r?.name ?? ''
    description.value = r?.description ?? ''
    parents.value = [...(r?.parents ?? [])]
    rules.value = (r?.rules ?? []).map((x) => withUid({ effect: x.effect, object: x.object, action: x.action }))
    if (roleId.value !== 'new' && !r) error.value = t('roles.notFound')
  } catch (e) {
    if (id === org.value?.id) error.value = message(e)
  }
}

async function save() {
  if (badEffects.value.length) return
  busy.value = true
  error.value = ''
  const isNew = roleId.value === 'new'
  try {
    const body = { org: org.value?.id, name: name.value.trim(), description: description.value, rules: rules.value.map(({ effect, object, action }) => ({ effect, object, action })), parents: parents.value }
    if (isNew) await iamClient.createRole(body)
    else await iamClient.updateRole({ ...body, role: roleId.value })
    await reload()
    await router.push(`/orgs/${org.value?.id}/roles`)
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = false
  }
}

watch(
  [() => org.value?.id, roleId],
  () => {
    error.value = ''
    searches.value = {}
    load()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <p v-if="current?.builtin" class="muted">{{ t('roles.builtinNote') }}</p>
    <p v-if="error" role="alert">{{ error }}</p>
    <p v-if="badEffects.length" role="alert">{{ t('roles.unknownEffect', { effects: badEffects.join(', ') }) }}</p>

    <Section :label="t('nav.general')">
      <Card>
        <TextInput v-if="current?.builtin" :model-value="name" :aria-label="t('members.name')" disabled />
        <template v-else>
          <TextInput v-model="name" :placeholder="t('members.name')" :aria-label="t('members.name')" :disabled="readonly" />
          <Textarea v-model="description" :placeholder="t('roles.description')" :aria-label="t('roles.description')" :disabled="readonly" />
        </template>
      </Card>
    </Section>

    <Section v-if="!current?.builtin" :label="t('roles.parents')">
      <Card>
        <div v-if="parents.length" class="row">
          <Tag v-for="p in parents" :key="p">
            {{ parentLabel(p) }}
            <Button v-if="!readonly" size="sm" variant="ghost" :aria-label="t('roles.removeRule')" @click="parents = parents.filter((x) => x !== p)">x</Button>
          </Tag>
        </div>
        <Combobox
          v-if="!readonly"
          v-model="addParent"
          :items="parentOptions"
          :placeholder="t('roles.addParent')"
          @select="(v) => { parents = [...parents, v]; addParent = '' }"
        />
      </Card>
    </Section>

    <Section :label="t('roles.rules')">
      <Card>
        <div v-for="(rule, i) in rules" :key="rule.uid" class="row">
          <SegmentedControl v-model="rule.effect" :options="effectOptions" :disabled="readonly" />
          <Tag v-if="badEffects.includes(rule.effect)">{{ rule.effect }}</Tag>
          <Combobox
            v-model="rule.object"
            v-model:search="searches[`o${rule.uid}`]"
            :items="suggestions(objects, `o${rule.uid}`)"
            :placeholder="t('roles.object')"
            :disabled="readonly"
          />
          <Combobox
            v-model="rule.action"
            v-model:search="searches[`a${rule.uid}`]"
            :items="suggestions(actions, `a${rule.uid}`, hints)"
            :placeholder="t('audit.action')"
            :disabled="readonly"
          />
          <Tag v-if="unknownRules[i]" tone="warning" :title="t('roles.unknownAction')" role="img" :aria-label="t('roles.unknownAction')">
            <TriangleAlert :size="12" aria-hidden="true" />
          </Tag>
          <Button v-if="!readonly" size="sm" variant="danger" @click="rules.splice(i, 1)">{{ t('roles.removeRule') }}</Button>
        </div>
        <p v-if="!readonly"><Button @click="rules.push(withUid({ effect: 'allow', object: '', action: '' }))">{{ t('roles.addRule') }}</Button></p>
      </Card>
    </Section>

    <div class="row">
      <Button @click="router.push(`/orgs/${org.id}/roles`)">{{ t('roles.back') }}</Button>
      <Button v-if="!readonly" variant="accent" :disabled="busy || !name.trim() || badEffects.length > 0" @click="save">{{ t('org.save') }}</Button>
    </div>
  </template>
</template>

<style scoped>
.row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
</style>
