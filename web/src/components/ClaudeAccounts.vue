<script setup lang="ts">
import { Button, Switch } from '@asterism-dev/lumen'
import { GripVertical } from '@lucide/vue'
import { computed, nextTick, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import type { ClaudeAccount, Integration } from '@/composables/integrations'
import { Meter, ResourceList, ResourceRow, Tag } from '@/ui'

const props = defineProps<{ orgId: string; integration: Integration }>()
const emit = defineEmits<{
  changed: []
  signIn: [ClaudeAccount]
  remove: [ClaudeAccount]
}>()

const { t, locale } = useI18n()
const ordered = ref<ClaudeAccount[]>([])
const error = ref('')
const dragId = ref('')
const root = ref<HTMLElement>()

watch(
  () => props.integration.accounts,
  (a) => (ordered.value = [...(a ?? [])].sort((x, y) => x.position - y.position)),
  { immediate: true },
)

const base = computed(() => `/api/orgs/${props.orgId}/integrations/${props.integration.id}/accounts`)
const label = (a: ClaudeAccount) => a.account || t('integrations.setupToken')
const lastLeft = computed(() => ordered.value.length === 1)
const canRemove = computed(() => props.integration.canEdit && (!lastLeft.value || props.integration.canDelete))

const limitedUntil = computed(() => {
  const times = ordered.value.filter((a) => a.status !== 'expired').map((a) => (a.usage.limitedUntil ? Date.parse(a.usage.limitedUntil) : 0))
  return ordered.value.length > 1 && times.length > 0 && times.every((v) => v > Date.now()) ? new Date(Math.min(...times)) : null
})

const errorKeys: Record<string, string> = {
  'no usage data': 'integrations.noUsageData',
  'usage poll failed': 'integrations.pollFailed',
  'refresh failed': 'integrations.refreshFailed',
}
const errorText = (e: string) => (errorKeys[e] ? t(errorKeys[e]) : e)

const switchNote = computed(() => {
  const s = props.integration.lastSwitch
  return s
    ? t('integrations.switchedNote', {
        time: time(s.at),
        from: s.from || t('integrations.setupToken'),
      })
    : ''
})

function time(iso: string | null) {
  if (!iso) return '–'
  const d = new Date(iso)
  const sameDay = d.toDateString() === new Date().toDateString()
  return d.toLocaleString(locale.value, sameDay ? { timeStyle: 'short' } : { dateStyle: 'short', timeStyle: 'short' })
}

function age(a: ClaudeAccount) {
  if (!a.usage.at) return ''
  const secs = Math.max(0, (Date.now() - Date.parse(a.usage.at)) / 1000)
  if (a.usage.source === 'proxy' && secs < 60) return t('usage.live')
  const rtf = new Intl.RelativeTimeFormat(locale.value, {
    numeric: 'auto',
    style: 'short',
  })
  const mins = Math.round(secs / 60)
  if (mins < 60) return rtf.format(-mins, 'minute')
  return mins < 1440 ? rtf.format(-Math.round(mins / 60), 'hour') : rtf.format(-Math.round(mins / 1440), 'day')
}

const fail = (e: unknown) => (error.value = e instanceof Error ? e.message : String(e))

async function activate(a: ClaudeAccount, on: boolean) {
  if (!on || a.active) return
  error.value = ''
  try {
    await api(`${base.value}/${a.id}/activate`, { method: 'POST' })
  } catch (e) {
    fail(e)
  }
  emit('changed')
}

async function move(id: string, to: number) {
  const from = ordered.value.findIndex((a) => a.id === id)
  if (from < 0 || to < 0 || to >= ordered.value.length || to === from) return
  const next = [...ordered.value]
  next.splice(to, 0, ...next.splice(from, 1))
  ordered.value = next
  error.value = ''
  try {
    await api(`${base.value}/order`, {
      method: 'PUT',
      body: JSON.stringify({ ids: next.map((a) => a.id) }),
    })
  } catch (e) {
    fail(e)
    emit('changed')
  }
}

async function onKey(e: KeyboardEvent, id: string) {
  const step = e.key === 'ArrowUp' ? -1 : e.key === 'ArrowDown' ? 1 : 0
  if (!step) return
  e.preventDefault()
  await move(id, ordered.value.findIndex((a) => a.id === id) + step)
  await nextTick()
  root.value?.querySelector<HTMLElement>(`[data-handle="${id}"]`)?.focus()
}

function onDragStart(e: DragEvent, id: string) {
  dragId.value = id
  if (!e.dataTransfer) return
  e.dataTransfer.effectAllowed = 'move'
  e.dataTransfer.setData('text/plain', id)
  const item = (e.target as HTMLElement).closest('li')
  if (item) e.dataTransfer.setDragImage(item, 0, 0)
}

function onDrop(id: string) {
  const id0 = dragId.value
  dragId.value = ''
  if (id0 && id0 !== id)
    move(
      id0,
      ordered.value.findIndex((a) => a.id === id),
    )
}
</script>

<template>
  <div ref="root" class="accounts">
    <p v-if="switchNote" class="note" role="status">{{ switchNote }}</p>
    <p v-if="limitedUntil" class="note" role="alert">
      {{ t('integrations.allLimited', { time: time(limitedUntil.toISOString()) }) }}
    </p>
    <p v-if="error" role="alert">{{ error }}</p>
    <ResourceList :count="ordered.length">
      <li v-for="a in ordered" :key="a.id" class="acct" :class="{ dragging: dragId === a.id }" @dragover.prevent @drop.prevent="onDrop(a.id)">
        <button
          v-if="integration.canEdit && ordered.length > 1"
          type="button"
          class="handle"
          draggable="true"
          :data-handle="a.id"
          :aria-label="t('integrations.reorder', { account: label(a) })"
          @dragstart="onDragStart($event, a.id)"
          @dragend="dragId = ''"
          @keydown="onKey($event, a.id)"
        >
          <GripVertical :size="16" aria-hidden="true" />
        </button>
        <ul class="inner">
          <ResourceRow :title="label(a)" :tone="a.status === 'expired' ? 'danger' : undefined">
            <template v-if="a.status === 'expired' || a.lastError" #tags>
              <Tag v-if="a.status === 'expired'" tone="danger" :title="a.lastError">{{ t('integrations.expired') }}</Tag>
              <Tag v-else :tone="a.lastError === 'no usage data' ? 'muted' : 'warning'">{{ errorText(a.lastError) }}</Tag>
            </template>
            <span class="usage">
              <Meter :label="t('integrations.fiveHour')" :percent="a.usage.fiveHourPct" />
              <span v-if="a.usage.fiveHourResetsAt" class="reset">{{
                t('integrations.resets', {
                  time: time(a.usage.fiveHourResetsAt),
                })
              }}</span>
              <Meter :label="t('integrations.week')" :percent="a.usage.weekPct" />
              <span v-if="a.usage.weekResetsAt" class="reset">{{ t('integrations.resets', { time: time(a.usage.weekResetsAt) }) }}</span>
              <span class="age">{{ age(a) }}</span>
            </span>
            <template v-if="integration.canEdit" #actions>
              <Switch :model-value="a.active" :disabled="a.active" :aria-label="t('integrations.useAccount', { account: label(a) })" @update:model-value="(v) => activate(a, v)" />
              <Button size="sm" variant="ghost" @click="emit('signIn', a)">{{ t('integrations.signInAgain') }}</Button>
              <Button v-if="canRemove" size="sm" variant="ghost" @click="emit('remove', a)">{{ t('integrations.remove') }}</Button>
            </template>
          </ResourceRow>
        </ul>
      </li>
    </ResourceList>
  </div>
</template>

<style scoped>
.accounts {
  display: grid;
  gap: 8px;
  padding-left: 24px;
}
.note {
  margin: 0;
  color: var(--muted);
  font-size: 0.8rem;
}
.acct {
  display: flex;
  align-items: center;
  gap: 8px;
}
.acct.dragging {
  opacity: 0.5;
}
.inner {
  flex: 1;
  min-width: 0;
  margin: 0;
  padding: 0;
  list-style: none;
}
.handle {
  display: inline-flex;
  padding: 4px;
  border: 0;
  background: none;
  color: var(--muted);
  cursor: grab;
}
.handle:hover,
.handle:focus-visible {
  color: var(--text);
}
.usage {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px 12px;
}
.reset,
.age {
  font-family: var(--font-mono);
  font-size: 0.7rem;
}
</style>
