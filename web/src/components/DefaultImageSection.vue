<script setup lang="ts">
import { Button } from '@asterism-dev/lumen'
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import ImagePicker from '@/components/ImagePicker.vue'
import { defaultSourceLabel, type ImageDefault } from '@/composables/images'
import { Card, KeyValue, KeyValueList, Section } from '@/ui'

const props = defineProps<{ orgId: string; level: string; editable: boolean; description?: string }>()

const { t } = useI18n()
const current = ref<ImageDefault | null>(null)
const picked = ref('')
const busy = ref(false)
const error = ref('')

const url = () => `/api/orgs/${props.orgId}/image-defaults/${props.level}`

async function load() {
  try {
    current.value = await api<ImageDefault>(url())
    // The picker lists latest as the bare reference.
    picked.value = current.value.configured?.ref.replace(/:latest$/, '') ?? ''
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

async function run(init: RequestInit) {
  busy.value = true
  error.value = ''
  try {
    await api(url(), init)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
  await load()
}

watch(() => [props.orgId, props.level], load, { immediate: true })
</script>

<template>
  <Section :label="t('images.defaultTitle')" :description="description ?? t('images.defaultHint')">
    <Card>
      <KeyValueList v-if="current">
        <KeyValue :label="t('images.defaultConfigured')">
          <code v-if="current.configured">{{ current.configured.ref }}</code>
          <span v-else class="muted">–</span>
          <span v-if="current.configured?.removed" class="danger">{{ t('images.defaultRemoved') }}</span>
        </KeyValue>
        <KeyValue :label="t('images.defaultEffective')">
          <code>{{ current.effective.ref }}</code>
          <span class="muted">{{ defaultSourceLabel(current.effective.source, t) }}</span>
        </KeyValue>
      </KeyValueList>
      <p v-if="editable" class="row">
        <ImagePicker v-model="picked" :org-id="orgId" :disabled="busy" />
        <Button variant="accent" :disabled="busy || !picked || picked === current?.configured?.ref.replace(/:latest$/, '')" @click="run({ method: 'PUT', body: JSON.stringify({ ref: picked }) })">{{ t('env.save') }}</Button>
        <Button v-if="current?.configured" :disabled="busy" @click="run({ method: 'DELETE' })">{{ t('images.defaultClear') }}</Button>
      </p>
      <p v-if="error" role="alert">{{ error }}</p>
    </Card>
  </Section>
</template>

<style scoped>
.row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
.danger {
  color: var(--danger);
}
</style>
