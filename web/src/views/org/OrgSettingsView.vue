<script setup lang="ts">
import { Button, TextInput } from '@asterism-dev/lumen'
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import { useMe } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'

const { t } = useI18n()
const { org, can, reload } = useOrg()
const { load: loadMe } = useMe()

const name = ref('')
const busy = ref(false)
const error = ref('')

watch(() => org.value?.name, (n) => (name.value = n ?? ''), { immediate: true })

async function save() {
  if (!org.value) return
  busy.value = true
  error.value = ''
  try {
    await api(`/api/orgs/${org.value.id}`, { method: 'PATCH', body: JSON.stringify({ name: name.value.trim() }) })
    await Promise.all([reload(), loadMe()])
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <template v-if="org">
    <p v-if="org.personal">{{ t('org.personalNote') }}</p>
    <div class="row">
      <TextInput v-model="name" :aria-label="t('org.name')" :disabled="org.personal || !can('org:update') || busy" />
      <Button
        variant="accent"
        :disabled="org.personal || !can('org:update') || busy || !name.trim() || name.trim() === org.name"
        @click="save"
      >
        {{ t('org.save') }}
      </Button>
    </div>
    <p v-if="error" role="alert">{{ error }}</p>
  </template>
</template>

<style scoped>
.row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 12px 0;
}
</style>
