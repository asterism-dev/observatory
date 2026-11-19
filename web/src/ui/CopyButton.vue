<script setup lang="ts">
import { Check, Copy } from '@lucide/vue'
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'

const props = defineProps<{ text: string; label?: boolean }>()
const { t } = useI18n()
const state = ref<'idle' | 'copied' | 'failed'>('idle')

async function copy() {
  try {
    await navigator.clipboard.writeText(props.text)
    state.value = 'copied'
  } catch {
    state.value = 'failed'
  }
  window.setTimeout(() => (state.value = 'idle'), 1500)
}
</script>

<template>
  <button type="button" class="copy" :class="{ 'copy--label': label }" :aria-label="t('ui.copy')" :title="state === 'failed' ? t('ui.copyFailed') : t('ui.copy')" @click.stop="copy">
    <Check v-if="state === 'copied'" :size="13" />
    <Copy v-else :size="13" />
    <span v-if="label">{{ state === 'copied' ? t('ui.copied') : state === 'failed' ? t('ui.copyFailed') : t('ui.copy') }}</span>
  </button>
</template>

<style scoped>
.copy {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 2px 4px;
  border: 0;
  border-radius: var(--radius);
  background: none;
  color: var(--muted);
  font: inherit;
  cursor: pointer;
}
.copy:hover {
  color: var(--text);
}
.copy--label {
  padding: 3px 8px;
  border: 1px solid var(--border);
  font-family: var(--font-mono);
  font-size: 0.7rem;
  letter-spacing: 0.06em;
  text-transform: uppercase;
}
.copy:focus-visible {
  outline: 2px solid var(--accent);
  outline-offset: 1px;
}
</style>
