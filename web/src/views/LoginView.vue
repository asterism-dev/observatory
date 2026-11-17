<script setup lang="ts">
import { Button } from '@asterism-dev/lumen'
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'

const { t } = useI18n()
const route = useRoute()

function go(register: boolean) {
  const redirect = typeof route.query.redirect === 'string' ? route.query.redirect : '/'
  location.href = '/auth/login?redirect=' + encodeURIComponent(redirect) + (register ? '&register=1' : '')
}
</script>

<template>
  <main class="login">
    <h1>{{ t('app.name') }}</h1>
    <p v-if="route.query.error" role="alert">{{ t('login.failed') }}</p>
    <p>{{ t('login.intro') }}</p>
    <div class="actions">
      <Button variant="accent" @click="go(false)">{{ t('login.signIn') }}</Button>
      <Button @click="go(true)">{{ t('login.createAccount') }}</Button>
    </div>
  </main>
</template>

<style scoped>
.login {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 12px;
  min-height: 100%;
  padding: 16px;
  text-align: center;
}
.actions {
  display: flex;
  gap: 8px;
}
</style>
