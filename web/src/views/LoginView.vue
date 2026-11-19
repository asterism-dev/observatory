<script setup lang="ts">
import { Button } from '@asterism-dev/lumen'
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'

import { Card } from '@/ui'

const { t } = useI18n()
const route = useRoute()

function go(register: boolean) {
  const redirect = typeof route.query.redirect === 'string' ? route.query.redirect : '/'
  location.href = '/auth/login?redirect=' + encodeURIComponent(redirect) + (register ? '&register=1' : '')
}
</script>

<template>
  <main class="login">
    <Card class="login__card">
      <h1>{{ t('app.name') }}</h1>
      <p v-if="route.query.error" role="alert">{{ t('login.failed') }}</p>
      <p class="muted">{{ t('login.intro') }}</p>
      <div class="actions">
        <Button variant="accent" @click="go(false)">{{ t('login.signIn') }}</Button>
        <Button @click="go(true)">{{ t('login.createAccount') }}</Button>
      </div>
    </Card>
  </main>
</template>

<style scoped>
.login {
  display: grid;
  place-items: center;
  min-height: 100%;
  padding: 16px;
  text-align: center;
}
.login__card {
  justify-items: center;
  width: min(100%, 360px);
  padding: 32px 24px;
}
h1 {
  margin: 0;
  font-size: 1.4rem;
}
.actions {
  display: flex;
  flex-wrap: wrap;
  justify-content: center;
  gap: 8px;
}
</style>
