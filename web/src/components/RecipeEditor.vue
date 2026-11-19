<script setup lang="ts">
import { Button, Textarea, TextInput } from '@asterism-dev/lumen'
import { File as FileIcon, Folder, Upload } from '@lucide/vue'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api, ApiError } from '@/api'
import RecipeRefsPicker from '@/components/RecipeRefsPicker.vue'
import { formatBytes, type Image, type Recipe, type RecipeRef } from '@/composables/images'
import { Card, EmptyState, Section } from '@/ui'

interface Entry {
  path: string
  mode: number
  size: number
  content: string | null
  upload?: File
  dirty: boolean
  saved: boolean
}

const props = defineProps<{ orgId: string; image: Image; base: string }>()
const emit = defineEmits<{ saved: [] }>()

const { t } = useI18n()
const revision = ref<number | null>(null)
const entries = ref<Entry[]>([])
const deleted = ref<string[]>([])
const refs = ref<RecipeRef[]>([])
const savedRefs = ref('')
const selected = ref('')
const newPath = ref('')
const busy = ref(false)
const error = ref('')
const conflict = ref(false)
const loaded = ref(false)
const uploadInput = ref<HTMLInputElement | null>(null)
const replaceInput = ref<HTMLInputElement | null>(null)

const readOnly = computed(() => !props.image.canEdit)
const current = computed(() => entries.value.find((e) => e.path === selected.value))
const refsDirty = computed(() => JSON.stringify(refs.value) !== savedRefs.value)
const dirty = computed(() => refsDirty.value || deleted.value.length > 0 || entries.value.some((e) => e.dirty))

const tree = computed(() => {
  const rows: { key: string; depth: number; name: string; path?: string }[] = []
  const seen = new Set<string>()
  for (const e of [...entries.value].sort((a, b) => a.path.localeCompare(b.path))) {
    const parts = e.path.split('/')
    parts.slice(0, -1).forEach((name, i) => {
      const dir = parts.slice(0, i + 1).join('/')
      if (!seen.has(dir)) rows.push({ key: `${dir}/`, depth: i, name })
      seen.add(dir)
    })
    rows.push({ key: e.path, depth: parts.length - 1, name: parts.at(-1) ?? e.path, path: e.path })
  }
  return rows
})

async function load() {
  error.value = ''
  conflict.value = false
  try {
    const r = await api<Recipe>(`${props.base}/recipe`)
    revision.value = r.revision
    entries.value = r.files.map((f) => ({ ...f, dirty: false, saved: true }))
    deleted.value = []
    refs.value = r.refs
    savedRefs.value = JSON.stringify(r.refs)
    if (!current.value) selected.value = entries.value.find((e) => e.path === 'Dockerfile')?.path ?? entries.value[0]?.path ?? ''
    loaded.value = true
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

function touch(e: Entry) {
  e.dirty = true
  e.size = e.upload?.size ?? new Blob([e.content ?? '']).size
}

function addEntry(path: string, upload?: File) {
  if (!entries.value.some((e) => e.path === path)) entries.value.push({ path, mode: 0o644, size: 0, content: '', dirty: true, saved: deleted.value.includes(path) })
  const e = entries.value.find((x) => x.path === path)!
  if (upload) {
    e.upload = upload
    e.content = null
  }
  deleted.value = deleted.value.filter((p) => p !== path)
  touch(e)
  selected.value = path
}

function newFile() {
  const path = newPath.value.trim()
  if (!path || path.endsWith('/')) return
  addEntry(path)
  newPath.value = ''
}

function uploaded(ev: Event) {
  const input = ev.target as HTMLInputElement
  const prefix = newPath.value.trim()
  for (const f of input.files ?? []) addEntry(prefix.endsWith('/') ? prefix + f.name : prefix && input.files?.length === 1 ? prefix : f.name, f)
  newPath.value = ''
  input.value = ''
}

function replaced(ev: Event) {
  const input = ev.target as HTMLInputElement
  const f = input.files?.[0]
  if (f && current.value) addEntry(current.value.path, f)
  input.value = ''
}

function remove(e: Entry) {
  entries.value = entries.value.filter((x) => x !== e)
  if (e.saved) deleted.value = [...deleted.value, e.path]
  selected.value = entries.value[0]?.path ?? ''
}

const toBase64 = (b: Blob) =>
  new Promise<string>((resolve, reject) => {
    const r = new FileReader()
    r.onload = () => resolve(String(r.result).slice(String(r.result).indexOf(',') + 1))
    r.onerror = () => reject(r.error)
    r.readAsDataURL(b)
  })

async function save() {
  busy.value = true
  error.value = ''
  conflict.value = false
  try {
    const put = await Promise.all(
      entries.value.filter((e) => e.dirty).map(async (e) => ({ path: e.path, mode: e.mode, content: await toBase64(e.upload ?? new Blob([e.content ?? ''])) })),
    )
    await api(`${props.base}/recipe/edits`, {
      method: 'POST',
      body: JSON.stringify({ base: revision.value ?? 0, put, delete: deleted.value, ...(refsDirty.value ? { refs: refs.value } : {}) }),
    })
    await load()
    emit('saved')
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
    conflict.value = e instanceof ApiError && e.status === 409
  } finally {
    busy.value = false
  }
}

watch(() => props.image.id, load, { immediate: true })
</script>

<template>
  <EmptyState
    v-if="loaded && !entries.length && readOnly && !deleted.length"
    :title="t('images.noRecipe')"
    :text="t('images.noRecipeText')"
    :command="`observatory image recipe push ${image.ref} ./recipe`"
  />
  <template v-else-if="loaded">
    <p v-if="error" role="alert">
      {{ error }}
      <Button v-if="conflict" size="sm" @click="load">{{ t('images.reload') }}</Button>
    </p>
    <Section :label="t('images.recipe')" :description="revision ? t('images.revision', { n: revision }) : t('images.noRecipeYet')">
      <template v-if="!readOnly" #actions>
        <span v-if="dirty" class="muted">{{ t('images.unsaved') }}</span>
        <Button size="sm" :disabled="busy || !dirty" @click="load">{{ t('images.discard') }}</Button>
        <Button size="sm" variant="accent" :disabled="busy || !dirty" @click="save">{{ t('env.save') }}</Button>
      </template>
      <div class="recipe">
        <Card flush class="recipe__tree">
          <ul>
            <li v-for="row in tree" :key="row.key" :style="{ paddingLeft: `${10 + row.depth * 14}px` }">
              <button v-if="row.path" type="button" :class="{ active: row.path === selected }" @click="selected = row.path">
                <FileIcon :size="13" />{{ row.name }}<span v-if="entries.find((e) => e.path === row.path)?.dirty" class="dot">•</span>
              </button>
              <span v-else class="dir"><Folder :size="13" />{{ row.name }}</span>
            </li>
            <li v-if="!tree.length" class="muted empty">{{ t('images.noFiles') }}</li>
          </ul>
          <form v-if="!readOnly" class="recipe__add" @submit.prevent="newFile">
            <TextInput v-model="newPath" :placeholder="t('images.newPath')" :aria-label="t('images.newPath')" />
            <span class="row">
              <Button size="sm" type="submit" :disabled="!newPath.trim() || newPath.trim().endsWith('/')">{{ t('images.newFile') }}</Button>
              <Button size="sm" :icon="Upload" @click="uploadInput?.click()">{{ t('images.upload') }}</Button>
            </span>
            <input ref="uploadInput" type="file" multiple hidden @change="uploaded" />
          </form>
        </Card>
        <Card class="recipe__file">
          <template v-if="current">
            <div class="row between">
              <code>{{ current.path }}</code>
              <span class="row">
                <span class="muted">{{ formatBytes(current.size) }}</span>
                <Button v-if="!readOnly" size="sm" variant="ghost" @click="replaceInput?.click()">{{ t('images.replace') }}</Button>
                <Button v-if="!readOnly" size="sm" variant="ghost" @click="remove(current)">{{ t('env.delete') }}</Button>
              </span>
              <input ref="replaceInput" type="file" hidden @change="replaced" />
            </div>
            <Textarea
              v-if="current.content !== null"
              v-model="current.content"
              class="code"
              :rows="22"
              spellcheck="false"
              :disabled="readOnly || busy"
              :aria-label="current.path"
              @update:model-value="touch(current)"
            />
            <p v-else class="muted">
              {{ current.upload ? t('images.uploadPending', { name: current.upload.name }) : t('images.binary') }}
              <a v-if="!current.upload" :href="`${base}/recipe/files/${current.path}`" :download="current.path.split('/').at(-1)">{{ t('images.download') }}</a>
            </p>
          </template>
          <p v-else class="muted">{{ t('images.pickFile') }}</p>
        </Card>
      </div>
    </Section>
    <Section :label="t('images.refs')" :description="t('images.refsHint')">
      <Card><RecipeRefsPicker v-model="refs" :org-id="orgId" :image="image" :disabled="readOnly || busy" /></Card>
    </Section>
  </template>
</template>

<style scoped>
.recipe {
  display: grid;
  grid-template-columns: minmax(200px, 1fr) minmax(0, 3fr);
  align-items: start;
  gap: 16px;
}
ul {
  margin: 0;
  padding: 6px 0;
  list-style: none;
}
li button,
.dir {
  display: flex;
  align-items: center;
  gap: 6px;
  width: 100%;
  padding: 4px 10px 4px 0;
  border: 0;
  background: none;
  color: var(--text);
  font-family: var(--font-mono);
  font-size: 0.8rem;
  text-align: left;
}
li button {
  cursor: pointer;
}
li button.active {
  color: var(--accent);
}
.dir {
  color: var(--muted);
}
.dot {
  color: var(--warning);
}
.empty {
  padding: 6px 10px;
}
.recipe__add {
  display: grid;
  gap: 6px;
  padding: 10px;
  border-top: 1px solid var(--border);
}
.row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px;
}
.between {
  justify-content: space-between;
}
.code :deep(textarea),
textarea.code {
  font-family: var(--font-mono);
  font-size: 0.8rem;
}
@media (max-width: 900px) {
  .recipe {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
