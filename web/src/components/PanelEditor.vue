<script setup lang="ts">
import { Button, Dialog, Select, TextInput } from '@asterism-dev/lumen'
import { Play } from '@lucide/vue'
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import DashboardPanel from '@/components/DashboardPanel.vue'
import { metricMatch, newPanel, panelError, panelTypes, panelUnits, wordAt, type Panel } from '@/composables/dashboards'

const props = defineProps<{ open: boolean; orgId: string; panel: Panel | null; adding: boolean; from: Date; to: Date }>()
const emit = defineEmits<{ 'update:open': [boolean]; save: [Panel] }>()

const { t } = useI18n()
const draft = ref<Panel>(newPanel())
const preview = ref<Panel | null>(null)
const tick = ref(0)
const area = ref<HTMLTextAreaElement | null>(null)
const suggestions = ref<string[]>([])
let token = { start: 0, word: '' }
let timer = 0
let seq = 0

const typeOptions = computed(() => panelTypes.map((v) => ({ value: v, label: t(`dashboards.types.${v}`) })))
const unitOptions = computed(() => panelUnits.map((v) => ({ value: v, label: t(`dashboards.units.${v}`) })))
const widthOptions = computed(() => ['half', 'full'].map((v) => ({ value: v, label: t(`dashboards.widths.${v}`) })))
const invalid = computed(() => panelError(draft.value))

watch(
  () => props.open,
  (o) => {
    if (!o) return
    draft.value = { ...(props.panel ?? newPanel()) }
    preview.value = null
    suggestions.value = []
  },
)

function run() {
  preview.value = { ...draft.value }
  tick.value++
}

// Debounced: one metric-names call per pause in typing, for the name being typed at the cursor.
function suggest() {
  window.clearTimeout(timer)
  const el = area.value
  if (!el) return
  token = wordAt(el.value, el.selectionStart)
  const n = ++seq
  if (token.word.length < 2) {
    suggestions.value = []
    return
  }
  const q = new URLSearchParams({ match: metricMatch(token.word) })
  timer = window.setTimeout(async () => {
    try {
      const res = await api<{ names: string[] }>(`/api/orgs/${props.orgId}/dashboards/metric-names?${q}`)
      if (n === seq) suggestions.value = res.names.filter((x) => x !== token.word).slice(0, 8)
    } catch {
      if (n === seq) suggestions.value = []
    }
  }, 250)
}

async function pick(name: string) {
  const q = draft.value.query
  const end = token.start + token.word.length
  draft.value.query = q.slice(0, token.start) + name + q.slice(end)
  suggestions.value = []
  await nextTick()
  area.value?.focus()
  area.value?.setSelectionRange(token.start + name.length, token.start + name.length)
}

// Esc closes the suggestions first; with none open it reaches the dialog.
function closeSuggestions(e: KeyboardEvent) {
  if (!suggestions.value.length) return
  e.stopPropagation()
  suggestions.value = []
}

function save() {
  emit('save', { ...draft.value })
  emit('update:open', false)
}

onBeforeUnmount(() => window.clearTimeout(timer))
</script>

<template>
  <Dialog :open="open" :title="t(adding ? 'dashboards.addPanel' : 'dashboards.editPanel')" width="720px" @update:open="(o) => emit('update:open', o)">
    <div class="form">
      <label>
        {{ t('dashboards.panelTitle') }}
        <TextInput v-model="draft.title" :aria-label="t('dashboards.panelTitle')" :invalid="invalid === 'title'" />
      </label>
      <div class="row">
        <label>
          {{ t('dashboards.type') }}
          <Select v-model="draft.type" :options="typeOptions" :aria-label="t('dashboards.type')" />
        </label>
        <label>
          {{ t('dashboards.unit') }}
          <Select v-model="draft.unit" :options="unitOptions" :aria-label="t('dashboards.unit')" />
        </label>
        <label>
          {{ t('dashboards.width') }}
          <Select v-model="draft.width" :options="widthOptions" :aria-label="t('dashboards.width')" />
        </label>
      </div>
      <label>
        {{ t('dashboards.query') }}
        <textarea
          ref="area"
          v-model="draft.query"
          class="lm-textarea mono"
          rows="4"
          spellcheck="false"
          autocomplete="off"
          :aria-label="t('dashboards.query')"
          :aria-invalid="invalid === 'query' && !!draft.query"
          :placeholder="t('dashboards.queryPlaceholder')"
          @input="suggest"
          @keydown.ctrl.enter.prevent="run"
          @keydown.meta.enter.prevent="run"
          @keydown.esc="closeSuggestions"
        />
      </label>
      <div v-if="suggestions.length" class="suggest" role="listbox" :aria-label="t('dashboards.metricNames')">
        <button v-for="s in suggestions" :key="s" type="button" role="option" class="mono" @click="pick(s)">{{ s }}</button>
      </div>
      <small class="muted">{{ t('dashboards.queryHint') }}</small>
      <div>
        <Button size="sm" :icon="Play" :disabled="!draft.query.trim()" @click="run">{{ t('dashboards.run') }}</Button>
      </div>
      <DashboardPanel v-if="preview" :org-id="orgId" :panel="preview" :from="from" :to="to" :tick="tick" sync-key="preview" />
    </div>
    <template #actions>
      <Button @click="emit('update:open', false)">{{ t('profile.cancel') }}</Button>
      <Button variant="accent" :disabled="!!invalid" @click="save">{{ t('dashboards.apply') }}</Button>
    </template>
  </Dialog>
</template>

<style scoped>
.form {
  display: grid;
  gap: 10px;
  min-width: 0;
}
label {
  display: grid;
  gap: 4px;
  min-width: 0;
}
.row {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
  gap: 8px;
}
textarea {
  width: 100%;
  box-sizing: border-box;
  resize: vertical;
}
.suggest {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}
.suggest button {
  padding: 2px 8px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--panel);
  color: var(--text);
  font-size: 0.75rem;
  cursor: pointer;
  overflow-wrap: anywhere;
}
small {
  font-size: 0.75rem;
}
</style>
