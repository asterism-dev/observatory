<script setup lang="ts">
import { Button, Dialog, TextInput } from '@asterism-dev/lumen'
import { Copy } from '@lucide/vue'
import QRCode from 'qrcode'
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'

const props = defineProps<{ open: boolean; orgId: string }>()
const emit = defineEmits<{ 'update:open': [boolean]; created: [] }>()

const { t } = useI18n()
const name = ref('')
const deviceName = ref('')
const config = ref('')
const qr = ref('')
const busy = ref(false)
const error = ref('')
const qrError = ref(false)
const copyError = ref(false)

watch(
  () => props.open,
  () => {
    name.value = ''
    deviceName.value = ''
    config.value = ''
    qr.value = ''
    error.value = ''
    qrError.value = false
    copyError.value = false
  },
)

async function create() {
  busy.value = true
  error.value = ''
  try {
    const res = await api<{ device: { name: string }; config: string }>(`/api/orgs/${props.orgId}/devices`, {
      method: 'POST',
      body: JSON.stringify({ name: name.value.trim() }),
    })
    deviceName.value = res.device.name
    config.value = res.config
    emit('created')
    qr.value = await QRCode.toString(res.config, { type: 'svg', margin: 1 }).catch(() => {
      qrError.value = true
      return ''
    })
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}

async function copy() {
  copyError.value = false
  try {
    await navigator.clipboard.writeText(config.value)
  } catch {
    copyError.value = true
  }
}

function download() {
  const url = URL.createObjectURL(new Blob([config.value], { type: 'text/plain' }))
  const a = document.createElement('a')
  a.href = url
  a.download = `${deviceName.value}.conf`
  a.click()
  URL.revokeObjectURL(url)
}
</script>

<template>
  <Dialog :open="open" :title="t('devices.addTitle')" @update:open="(o) => !busy && emit('update:open', o)">
    <template v-if="config">
      <p>{{ t('devices.configHint') }}</p>
      <p role="alert">{{ t('devices.configWarning') }}</p>
      <pre><code>{{ config }}</code></pre>
      <div class="row">
        <Button size="sm" @click="download">{{ t('devices.download') }}</Button>
        <Button size="sm" @click="copy"><Copy :size="14" /> {{ t('devices.copy') }}</Button>
      </div>
      <p v-if="copyError" role="alert">{{ t('devices.copyFailed') }}</p>
      <p v-if="qrError" role="alert">{{ t('devices.qrFailed') }}</p>
      <div v-else class="qr" role="img" :aria-label="t('devices.qr')" v-html="qr" />
    </template>
    <TextInput v-else v-model="name" :aria-label="t('devices.name')" :placeholder="t('devices.name')" @keydown.enter="name.trim() && !busy && create()" />
    <p v-if="error" role="alert">{{ error }}</p>
    <template #actions>
      <Button :disabled="busy" @click="emit('update:open', false)">{{ config ? t('devices.done') : t('profile.cancel') }}</Button>
      <Button v-if="!config" variant="accent" :disabled="busy || !name.trim()" @click="create">{{ t('devices.create') }}</Button>
    </template>
  </Dialog>
</template>

<style scoped>
pre {
  margin: 0 0 8px;
  overflow-x: auto;
  white-space: pre-wrap;
  word-break: break-all;
}
.row {
  display: flex;
  gap: 8px;
}
.qr {
  width: 200px;
  margin-top: 12px;
  background: #fff;
}
.qr :deep(svg) {
  display: block;
  width: 100%;
  height: auto;
}
</style>
