<script setup lang="ts">
import { Badge, Button, Combobox, SegmentedControl, Textarea, TextInput } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'

import { api } from '@/api'
import { useOrg } from '@/composables/useOrg'

interface Rule {
  uid: number
  effect: 'allow' | 'deny'
  object: string
  action: string
}
interface Role {
  id?: string
  key?: string
  name: string
  description?: string
  builtin?: boolean
  rules: Rule[]
  parents?: string[]
}
interface Perm {
  action: string
  object: string
}

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const { org, can, reload } = useOrg()

const roles = ref<Role[]>([])
const catalog = ref<Perm[]>([])
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
const current = computed(() => roles.value.find((r) => (r.key ?? r.id) === roleId.value))
const readonly = computed(() => !!current.value?.builtin || !can('role:write'))
const canDeny = computed(() => ['owner', 'admin'].includes(org.value?.role ?? ''))

const effectOptions = computed(() => [
  { value: 'allow' as const, label: t('roles.allow') },
  { value: 'deny' as const, label: t('roles.deny'), disabled: !canDeny.value },
])
const parentLabel = (p: string) => roles.value.find((r) => (r.key ?? r.id) === p)?.name ?? p
const parentOptions = computed(() =>
  roles.value
    .filter((r) => (r.id ? r.id !== roleId.value : r.key !== 'owner'))
    .map((r) => ({ value: r.key ?? r.id!, label: r.name }))
    .filter((o) => !parents.value.includes(o.value)),
)

const objects = computed(() => [...new Set(catalog.value.map((p) => p.object))])
const actions = computed(() => {
  const all = catalog.value.map((p) => p.action)
  const types = new Set(all.map((a) => a.split(':')[0] + ':*'))
  return ['*', ...types, ...new Set(all)]
})

// Combobox filters are the caller's job; the typed text is offered as a pick so free-form values work.
function suggestions(pool: string[], key: string) {
  const q = (searches.value[key] ?? '').trim()
  const hits = pool.filter((v) => v.toLowerCase().includes(q.toLowerCase())).slice(0, 50)
  if (q && !pool.includes(q)) hits.unshift(q)
  return hits.map((v) => ({ value: v, label: v }))
}

const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

async function load() {
  if (!org.value) return
  const id = org.value.id
  try {
    const base = `/api/orgs/${id}`
    const [rs, cat] = await Promise.all([api<Role[]>(`${base}/roles`), api<Perm[]>(`${base}/catalog`)])
    // Org switched while loading.
    if (id !== org.value?.id) return
    roles.value = rs
    catalog.value = cat
    const r = rs.find((x) => (x.key ?? x.id) === roleId.value)
    name.value = r?.name ?? ''
    description.value = r?.description ?? ''
    parents.value = [...(r?.parents ?? [])]
    rules.value = (r?.rules ?? []).map(withUid)
    if (roleId.value !== 'new' && !r) error.value = t('roles.notFound')
  } catch (e) {
    if (id === org.value?.id) error.value = message(e)
  }
}

async function save() {
  busy.value = true
  error.value = ''
  const isNew = roleId.value === 'new'
  try {
    const body = { name: name.value.trim(), description: description.value, rules: rules.value.map(({ effect, object, action }) => ({ effect, object, action })), parents: parents.value }
    await api(`/api/orgs/${org.value?.id}/roles${isNew ? '' : `/${roleId.value}`}`, {
      method: isNew ? 'POST' : 'PUT',
      body: JSON.stringify(body),
    })
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
    <h1>{{ roleId === 'new' ? t('roles.create') : name }}</h1>
    <p v-if="current?.builtin">{{ t('roles.builtinNote') }}</p>
    <p v-if="error" role="alert">{{ error }}</p>

    <template v-if="current?.builtin">
      <TextInput :model-value="name" :aria-label="t('members.name')" disabled />
    </template>
    <template v-else>
      <TextInput v-model="name" :placeholder="t('members.name')" :aria-label="t('members.name')" :disabled="readonly" />
      <Textarea v-model="description" :placeholder="t('roles.description')" :aria-label="t('roles.description')" :disabled="readonly" />

      <h2>{{ t('roles.parents') }}</h2>
      <div class="chips">
        <Badge v-for="p in parents" :key="p">
          {{ parentLabel(p) }}
          <Button v-if="!readonly" size="sm" variant="ghost" :aria-label="t('roles.removeRule')" @click="parents = parents.filter((x) => x !== p)">x</Button>
        </Badge>
      </div>
      <Combobox
        v-if="!readonly"
        v-model="addParent"
        :items="parentOptions"
        :placeholder="t('roles.addParent')"
        @select="(v) => { parents = [...parents, v]; addParent = '' }"
      />
    </template>

    <h2>{{ t('roles.rules') }}</h2>
    <div v-for="(rule, i) in rules" :key="rule.uid" class="rule">
      <SegmentedControl v-model="rule.effect" :options="effectOptions" :disabled="readonly" />
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
        :items="suggestions(actions, `a${rule.uid}`)"
        :placeholder="t('audit.action')"
        :disabled="readonly"
      />
      <Button v-if="!readonly" size="sm" variant="danger" @click="rules.splice(i, 1)">{{ t('roles.removeRule') }}</Button>
    </div>
    <Button v-if="!readonly" @click="rules.push(withUid({ effect: 'allow', object: '', action: '' }))">{{ t('roles.addRule') }}</Button>

    <div class="actions">
      <Button @click="router.push(`/orgs/${org.id}/roles`)">{{ t('roles.back') }}</Button>
      <Button v-if="!readonly" variant="accent" :disabled="busy || !name.trim()" @click="save">{{ t('org.save') }}</Button>
    </div>
  </template>
</template>

<style scoped>
.rule {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}
.chips,
.actions {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin: 12px 0;
}
</style>
