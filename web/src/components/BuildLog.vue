<script setup lang="ts">
import { Download } from '@lucide/vue'
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { appendCapped, buildActive, type Build, type LogView } from '@/composables/images'

const props = defineProps<{ base: string; build: Build }>()
const emit = defineEmits<{ ended: [] }>()

const { t } = useI18n()
const empty: LogView = { text: '', truncated: false }
const log = ref<LogView>(empty)
const error = ref('')
const pre = ref<HTMLElement | null>(null)
const following = ref(false)
let source: EventSource | null = null
let pending = ''
let timer = 0

const atBottom = () => !pre.value || pre.value.scrollHeight - pre.value.scrollTop - pre.value.clientHeight < 24

// Lines are batched so a fast build re-renders and measures the log a few times a second, not once per line.
async function flush() {
  window.clearTimeout(timer)
  timer = 0
  if (!pending) return
  const stick = atBottom()
  log.value = appendCapped(log.value, pending)
  pending = ''
  await nextTick()
  if (stick && pre.value) pre.value.scrollTop = pre.value.scrollHeight
}

function append(line: string) {
  pending += line + '\n'
  if (!timer) timer = window.setTimeout(flush, 100)
}

function reset() {
  window.clearTimeout(timer)
  timer = 0
  pending = ''
  log.value = empty
}

function stop() {
  source?.close()
  source = null
  following.value = false
}

async function load() {
  stop()
  reset()
  error.value = ''
  const url = `${props.base}/builds/${props.build.id}/log`
  if (!buildActive(props.build)) {
    try {
      const res = await fetch(url)
      if (!res.ok) throw new Error(res.statusText)
      log.value = appendCapped(empty, await res.text())
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    }
    return
  }
  following.value = true
  source = new EventSource(`${url}?follow=1`)
  // A reconnect replays the log from the start.
  source.onopen = reset
  source.onmessage = (e) => append(e.data)
  // While CONNECTING the browser retries by itself; CLOSED means the hub refused the stream.
  source.onerror = () => {
    if (source?.readyState !== EventSource.CLOSED) return
    stop()
    flush()
    error.value = t('images.logStreamFailed')
  }
  source.addEventListener('end', () => {
    stop()
    flush()
    emit('ended')
  })
}

watch(() => props.build.id, load, { immediate: true })
onBeforeUnmount(() => {
  stop()
  window.clearTimeout(timer)
})
</script>

<template>
  <div class="log">
    <div class="log__bar">
      <span class="muted">{{ following ? t('images.logLive') : t('images.logFull') }}</span>
      <a :href="`${base}/builds/${build.id}/log`" :download="`build-${build.id}.txt`" class="lm-button"><Download :size="16" :stroke-width="1.75" />{{ t('images.download') }}</a>
      <slot />
    </div>
    <p v-if="error" role="alert">{{ error }}</p>
    <pre ref="pre"><span v-if="log.truncated" class="muted">{{ t('images.logTruncated') }}{{ '\n' }}</span>{{ log.text || (following ? t('images.logWaiting') : t('images.logEmpty')) }}</pre>
  </div>
</template>

<style scoped>
.log {
  display: grid;
  gap: 8px;
}
.log__bar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
.log__bar > .muted {
  flex: 1;
}
.log__bar a {
  color: var(--text);
  text-decoration: none;
}
pre {
  max-height: 60vh;
  margin: 0;
  padding: 12px 14px;
  overflow: auto;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--bg);
  font-family: var(--font-mono);
  font-size: 0.75rem;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
</style>
