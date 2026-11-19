<script setup lang="ts">
import { Button } from '@asterism-dev/lumen'
import { Copy } from '@lucide/vue'
import { useI18n } from 'vue-i18n'

const props = defineProps<{ command: string }>()
const { t } = useI18n()
const copy = () => navigator.clipboard.writeText(props.command)
</script>

<template>
  <div class="box">
    <pre><code>{{ command }}</code></pre>
    <Button size="sm" :aria-label="t('nodes.copy')" @click="copy"><Copy :size="14" /></Button>
  </div>
  <p>{{ t('nodes.tokenNote') }}</p>
</template>

<style scoped>
.box {
  display: flex;
  align-items: flex-start;
  gap: 8px;
}
pre {
  flex: 1;
  margin: 0;
  overflow-x: auto;
  white-space: pre-wrap;
  word-break: break-all;
}
</style>
