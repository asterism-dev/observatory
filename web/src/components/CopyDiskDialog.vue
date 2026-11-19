<script setup lang="ts">
import { Button, Dialog, Select, TextInput } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'

const props = defineProps<{ open: boolean; orgId: string; diskId: string; diskName: string; unsettled: boolean }>()
const emit = defineEmits<{ 'update:open': [boolean]; copied: [] }>()

const { t } = useI18n()
const name = ref('')
const owner = ref('user')
const teams = ref<{ id: string; name: string }[]>([])
const busy = ref(false)
const error = ref('')
const warning = ref('')

const nameInvalid = computed(() => !/^[a-z0-9][a-z0-9-]{0,62}$/.test(name.value))
const ownerOptions = computed(() => [
  { value: 'user', label: t('vms.ownerMe') },
  ...teams.value.map((x) => ({ value: x.id, label: t('vms.ownerTeam', { name: x.name }) })),
])

watch(
  () => props.open,
  async (o) => {
    if (!o) return
    name.value = ''
    owner.value = 'user'
    error.value = ''
    warning.value = ''
    try {
      teams.value = await api(`/api/orgs/${props.orgId}/teams`)
    } catch {
      teams.value = []
    }
  },
)

async function copy() {
  busy.value = true
  error.value = ''
  try {
    const res = await api<{ warning?: string }>(`/api/orgs/${props.orgId}/disks/${props.diskId}/copy`, {
      method: 'POST',
      body: JSON.stringify({ name: name.value, owner: owner.value === 'user' ? { kind: 'user' } : { kind: 'team', teamId: owner.value } }),
    })
    emit('copied')
    if (res?.warning) warning.value = res.warning
    else emit('update:open', false)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Dialog :open="open" :title="t('disks.copyTitle', { name: diskName })" @update:open="(o) => !busy && emit('update:open', o)">
    <div v-if="!warning" class="form">
      <label>
        {{ t('disks.copyName') }}
        <TextInput v-model="name" :aria-label="t('disks.copyName')" :invalid="!!name && nameInvalid" />
      </label>
      <small v-if="name && nameInvalid" role="alert">{{ t('disks.invalidName') }}</small>
      <label>
        {{ t('disks.owner') }}
        <Select v-model="owner" :options="ownerOptions" :aria-label="t('disks.owner')" />
      </label>
      <small>{{ t('disks.copyHint') }}</small>
      <p v-if="unsettled" role="alert">{{ t('disks.copyUnsettled') }}</p>
    </div>
    <p v-else role="status">{{ t('disks.copyWarning') }} ({{ warning }})</p>
    <p v-if="error" role="alert">{{ error }}</p>
    <template #actions>
      <template v-if="!warning">
        <Button :disabled="busy" @click="emit('update:open', false)">{{ t('profile.cancel') }}</Button>
        <Button variant="accent" :disabled="busy || nameInvalid" @click="copy">{{ t('disks.copy') }}</Button>
      </template>
      <Button v-else variant="accent" @click="emit('update:open', false)">{{ t('nodes.done') }}</Button>
    </template>
  </Dialog>
</template>

<style scoped>
.form {
  display: grid;
  gap: 8px;
}
label {
  display: grid;
  gap: 4px;
}
</style>
