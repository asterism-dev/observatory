<script setup lang="ts">
import { Button, Select } from '@asterism-dev/lumen'
import { X } from '@lucide/vue'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import type { EnvVar } from '@/composables/envScopes'
import { refLabel, type Image, type RecipeRef } from '@/composables/images'
import type { Integration } from '@/composables/integrations'

const refs = defineModel<RecipeRef[]>({ required: true })
const props = defineProps<{ orgId: string; image: Image; disabled?: boolean }>()

const { t } = useI18n()
const candidates = ref<RecipeRef[]>([])
const teamNames = ref<Record<string, string>>({})
const picked = ref('')

const key = (r: RecipeRef) => [r.kind, r.scope, r.teamId ?? '', r.name].join('|')
// Mirrors the hub's scope rule: private images may use everything, team images their team and the org, org images the org.
const allowed = (r: RecipeRef) =>
  r.scope === 'org' || (r.scope === 'team' ? props.image.scope === 'user' || (props.image.scope === 'team' && r.teamId === props.image.ownerId) : props.image.scope === 'user')

const options = computed(() =>
  candidates.value
    .filter((c) => allowed(c) && !refs.value.some((r) => key(r) === key(c)))
    .map((c) => ({ value: key(c), label: refLabel(c, teamNames.value) })),
)

async function load() {
  const [env, igs] = await Promise.all([
    api<{ vars: EnvVar[] }>(`/api/orgs/${props.orgId}/env`).then((r) => r.vars, () => []),
    api<{ integrations: Integration[] }>(`/api/orgs/${props.orgId}/integrations`).then((r) => r.integrations, () => []),
  ])
  const names: Record<string, string> = {}
  for (const v of [...env, ...igs]) if (v.teamId && v.teamName) names[v.teamId] = v.teamName
  teamNames.value = names
  candidates.value = [
    ...env.filter((v) => v.kind === 'secret').map((v) => ({ kind: 'secret' as const, scope: v.scope, teamId: v.teamId, name: v.name })),
    ...igs.map((v) => ({ kind: 'integration' as const, scope: v.scope, teamId: v.teamId, name: v.kind })),
  ]
}

function add() {
  const c = candidates.value.find((x) => key(x) === picked.value)
  if (c) refs.value = [...refs.value, c]
  picked.value = ''
}

watch(() => props.orgId, load, { immediate: true })
</script>

<template>
  <div class="refs">
    <span v-for="r in refs" :key="key(r)" class="refs__item">
      <code>{{ refLabel(r, teamNames) }}</code>
      <button v-if="!disabled" type="button" class="refs__remove" :aria-label="t('images.removeRef')" @click="refs = refs.filter((x) => key(x) !== key(r))"><X :size="13" /></button>
    </span>
    <span v-if="!refs.length" class="muted">{{ t('images.noRefs') }}</span>
    <span v-if="!disabled && options.length" class="refs__add">
      <Select v-model="picked" :options="options" :placeholder="t('images.addRef')" :aria-label="t('images.addRef')" />
      <Button size="sm" :disabled="!picked" @click="add">{{ t('images.add') }}</Button>
    </span>
  </div>
</template>

<style scoped>
.refs {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
.refs__item {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 2px 6px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  font-family: var(--font-mono);
  font-size: 0.75rem;
}
.refs__remove {
  display: inline-flex;
  padding: 0;
  border: 0;
  background: none;
  color: var(--muted);
  cursor: pointer;
}
.refs__remove:hover {
  color: var(--text);
}
.refs__add {
  display: inline-flex;
  gap: 6px;
}
</style>
