<script setup lang="ts">
import { Button, TextInput } from '@asterism-dev/lumen'
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import AuditDataEventsSection from '@/components/AuditDataEventsSection.vue'
import DefaultImageSection from '@/components/DefaultImageSection.vue'
import { useMe } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'
import { Card, Section, ValueField } from '@/ui'

const { t } = useI18n()
const { org, can, reload } = useOrg()
const { load: loadMe } = useMe()

const name = ref('')
const editing = ref(false)
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
    editing.value = false
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Section v-if="org" :label="t('org.identity')" :description="t('org.identityHint')">
    <Card>
      <p v-if="org.personal" class="muted">{{ t('org.personalNote') }}</p>
      <ValueField :label="t('org.displayName')" :value="org.name">
        <template v-if="!org.personal && can('iam.orgs.update') && !editing" #actions>
          <Button size="sm" @click="editing = true">{{ t('org.edit') }}</Button>
        </template>
        <form v-if="editing" class="row" @submit.prevent="save">
          <TextInput v-model="name" :aria-label="t('org.displayName')" :disabled="busy" />
          <Button type="submit" variant="accent" :disabled="busy || !name.trim() || name.trim() === org.name">{{ t('org.save') }}</Button>
          <Button :disabled="busy" @click="(editing = false), (name = org.name)">{{ t('profile.cancel') }}</Button>
        </form>
      </ValueField>
      <ValueField :label="t('org.id')" :value="org.id" mono copy />
      <p v-if="error" role="alert">{{ error }}</p>
    </Card>
  </Section>
  <DefaultImageSection v-if="org" :org-id="org.id" level="org" :editable="can('artifacts.images.setDefault')" :description="t('images.orgDefaultHint')" />
  <AuditDataEventsSection v-if="org && can('audit.settings.get')" :key="org.id" :org="org.id" :editable="can('audit.settings.update')" />
</template>

<style scoped>
.row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
</style>
