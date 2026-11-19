<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'

import { api } from '@/api'
import { useMe } from '@/composables/useMe'
import { Card } from '@/ui'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const { load, selectOrg } = useMe()

const error = ref('')

onMounted(async () => {
  try {
    const { orgId } = await api<{ orgId: string; role: string }>('/api/invites/accept', {
      method: 'POST',
      body: JSON.stringify({ token: route.params.token }),
    })
    await load()
    selectOrg(orgId)
    await router.replace(`/orgs/${orgId}/dashboard`)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
})
</script>

<template>
  <main class="invite">
    <Card class="invite__card">
      <h1>{{ t('invite.title') }}</h1>
      <p v-if="error" role="alert">{{ error }}</p>
      <p v-else class="muted">{{ t('invite.accepting') }}</p>
      <RouterLink v-if="error" to="/">{{ t('invite.home') }}</RouterLink>
    </Card>
  </main>
</template>

<style scoped>
.invite {
  display: grid;
  place-items: center;
  min-height: 100%;
  padding: 16px;
  text-align: center;
}
.invite__card {
  justify-items: center;
  width: min(100%, 360px);
  padding: 32px 24px;
}
h1 {
  margin: 0;
  font-size: 1.4rem;
}
</style>
