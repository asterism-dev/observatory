<script setup lang="ts">
import { Button } from '@asterism-dev/lumen'
import { Ellipsis, GripVertical } from '@lucide/vue'
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
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
const menuFor = ref('')

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

async function activate(a: ClaudeAccount) {
  error.value = ''
  try {
    await api(`${base.value}/${a.id}/activate`, { method: 'POST' })
  } catch (e) {
    fail(e)
  }
  emit('changed')
}

function closeMenu(focus = false) {
  const id = menuFor.value
  menuFor.value = ''
  if (focus) root.value?.querySelector<HTMLElement>(`[data-menu="${id}"] > button`)?.focus()
}

async function toggleMenu(id: string) {
  menuFor.value = menuFor.value === id ? '' : id
  await nextTick()
  if (menuFor.value) root.value?.querySelector<HTMLElement>(`[data-menu="${id}"] [role="menuitem"]`)?.focus()
}

const onOutside = (e: PointerEvent) => menuFor.value && !(e.target as Element).closest(`[data-menu="${menuFor.value}"]`) && closeMenu()
onMounted(() => document.addEventListener('pointerdown', onOutside))
onBeforeUnmount(() => document.removeEventListener('pointerdown', onOutside))

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
            <template #title>
              <Tag v-if="a.active" tone="success">{{ t('integrations.active') }}</Tag>
              <span :title="label(a)">{{ label(a) }}</span>
            </template>
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
              <Button v-if="!a.active" size="sm" @click="activate(a)">{{ t('integrations.activate') }}</Button>
              <div class="menu" :data-menu="a.id" @keydown.esc="closeMenu(true)">
                <Button
                  size="sm"
                  variant="ghost"
                  :icon="Ellipsis"
                  aria-haspopup="menu"
                  :aria-expanded="menuFor === a.id"
                  :aria-label="t('integrations.moreActions', { account: label(a) })"
                  @click="toggleMenu(a.id)"
                />
                <ul v-if="menuFor === a.id" role="menu" class="menu__list">
                  <li role="none">
                    <button type="button" role="menuitem" @click="closeMenu(), emit('signIn', a)">{{ t('integrations.signInAgain') }}</button>
                  </li>
                  <li v-if="canRemove" role="none">
                    <button type="button" role="menuitem" class="danger" @click="closeMenu(), emit('remove', a)">{{ t('integrations.remove') }}</button>
                  </li>
                </ul>
              </div>
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
.menu {
  position: relative;
}
.menu__list {
  position: absolute;
  z-index: 10;
  top: calc(100% + 4px);
  right: 0;
  display: grid;
  min-width: 160px;
  margin: 0;
  padding: 4px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--panel);
  list-style: none;
  box-shadow: 0 4px 16px rgb(0 0 0 / 0.15);
}
.menu__list button {
  width: 100%;
  padding: 6px 10px;
  border: 0;
  border-radius: calc(var(--radius) - 2px);
  background: none;
  color: var(--text);
  font: inherit;
  text-align: left;
  cursor: pointer;
}
.menu__list button:hover,
.menu__list button:focus-visible {
  background: color-mix(in srgb, var(--text) 8%, transparent);
}
.menu__list .danger {
  color: var(--danger);
}
.reset,
.age {
  font-family: var(--font-mono);
  font-size: 0.7rem;
}
</style>
