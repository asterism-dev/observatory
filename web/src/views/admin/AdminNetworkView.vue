<script setup lang="ts">
import { Button, TextInput } from '@asterism-dev/lumen'
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import { Card, Section } from '@/ui'

const { t } = useI18n()
const saved = ref(0)
const mtu = ref('')
const busy = ref(false)
const error = ref('')

const valid = () => /^\d+$/.test(mtu.value) && +mtu.value >= 1000 && +mtu.value <= 1362

async function load() {
  try {
    saved.value = (await api<{ guestMTU: number }>('/api/admin/network')).guestMTU
    mtu.value = String(saved.value)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

async function save() {
  busy.value = true
  error.value = ''
  try {
    saved.value = (await api<{ guestMTU: number }>('/api/admin/network', { method: 'PUT', body: JSON.stringify({ guestMTU: +mtu.value }) })).guestMTU
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}

onMounted(load)
</script>

<template>
  <Section :label="t('adminNetwork.title')">
    <Card>
      <p>{{ t('adminNetwork.guestMtuHint') }}</p>
      <form class="row" @submit.prevent="save">
        <TextInput v-model="mtu" inputmode="numeric" :aria-label="t('adminNetwork.guestMtu')" :disabled="busy" />
        <Button type="submit" variant="accent" :disabled="busy || !valid() || +mtu === saved">{{ t('org.save') }}</Button>
      </form>
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
</style>
