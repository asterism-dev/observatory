<script setup lang="ts">
import { Button, Dialog, NumberInput, Select, TextInput } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import { iamClient } from '@/composables/iamClient'

const props = defineProps<{ open: boolean; orgId: string }>()
const emit = defineEmits<{ 'update:open': [boolean]; created: [] }>()

const { t } = useI18n()
const name = ref('')
const owner = ref('user')
const sizeGB = ref<number | null>(10)
const teams = ref<{ id: string; name: string }[]>([])
const busy = ref(false)
const error = ref('')

const nameInvalid = computed(() => !/^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/.test(name.value))
const sizeInvalid = computed(() => sizeGB.value === null || !Number.isInteger(sizeGB.value) || sizeGB.value < 1 || sizeGB.value > 4096)
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
    sizeGB.value = 10
    error.value = ''
    try {
      teams.value = (await iamClient.listTeams({ org: props.orgId })).teams
    } catch {
      teams.value = []
    }
  },
)

async function create() {
  busy.value = true
  error.value = ''
  try {
    await api(`/api/orgs/${props.orgId}/disks`, {
      method: 'POST',
      body: JSON.stringify({
        name: name.value,
        owner: owner.value === 'user' ? { kind: 'user' } : { kind: 'team', teamId: owner.value },
        sizeGB: sizeGB.value,
      }),
    })
    emit('created')
    emit('update:open', false)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Dialog :open="open" :title="t('disks.createTitle')" @update:open="(o) => !busy && emit('update:open', o)">
    <div class="form">
      <label>
        {{ t('disks.name') }}
        <TextInput v-model="name" :aria-label="t('disks.name')" :invalid="!!name && nameInvalid" />
      </label>
      <small v-if="name && nameInvalid" role="alert">{{ t('disks.invalidName') }}</small>
      <label>
        {{ t('disks.owner') }}
        <Select v-model="owner" :options="ownerOptions" :aria-label="t('disks.owner')" />
      </label>
      <label>
        {{ t('disks.size') }}
        <NumberInput v-model="sizeGB" :aria-label="t('disks.size')" />
      </label>
      <small v-if="sizeInvalid" role="alert">{{ t('disks.invalidSize') }}</small>
    </div>
    <p v-if="error" role="alert">{{ error }}</p>
    <template #actions>
      <Button :disabled="busy" @click="emit('update:open', false)">{{ t('profile.cancel') }}</Button>
      <Button variant="accent" :disabled="busy || nameInvalid || sizeInvalid" @click="create">{{ t('devices.create') }}</Button>
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
