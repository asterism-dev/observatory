<script setup lang="ts">
import { Download } from '@lucide/vue'
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { buildActive, type Build } from '@/composables/images'

const props = defineProps<{ base: string; build: Build }>()
const emit = defineEmits<{ ended: [] }>()

const { t } = useI18n()
const text = ref('')
const error = ref('')
const pre = ref<HTMLElement | null>(null)
const following = ref(false)
let source: EventSource | null = null

const atBottom = () => !pre.value || pre.value.scrollHeight - pre.value.scrollTop - pre.value.clientHeight < 24

async function append(line: string) {
  const stick = atBottom()
  text.value += line + '\n'
  await nextTick()
  if (stick && pre.value) pre.value.scrollTop = pre.value.scrollHeight
}

function stop() {
  source?.close()
  source = null
  following.value = false
}

async function load() {
  stop()
  text.value = ''
  error.value = ''
  const url = `${props.base}/builds/${props.build.id}/log`
  if (!buildActive(props.build)) {
    try {
      const res = await fetch(url)
      if (!res.ok) throw new Error(res.statusText)
      text.value = await res.text()
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    }
    return
  }
  following.value = true
  source = new EventSource(`${url}?follow=1`)
  // A reconnect replays the log from the start.
  source.onopen = () => (text.value = '')
  source.onmessage = (e) => append(e.data)
  source.addEventListener('end', () => {
    stop()
    emit('ended')
  })
}

watch(() => props.build.id, load, { immediate: true })
onBeforeUnmount(stop)
</script>

<template>
  <div class="log">
    <div class="log__bar">
      <span class="muted">{{ following ? t('images.logLive') : t('images.logFull') }}</span>
      <a :href="`${base}/builds/${build.id}/log`" :download="`build-${build.id}.txt`" class="lm-button"><Download :size="16" :stroke-width="1.75" />{{ t('images.download') }}</a>
      <slot />
    </div>
    <p v-if="error" role="alert">{{ error }}</p>
    <pre ref="pre">{{ text || (following ? t('images.logWaiting') : t('images.logEmpty')) }}</pre>
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
