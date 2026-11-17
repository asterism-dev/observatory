<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'

const { t } = useI18n()
const route = useRoute()
</script>

<template>
  <h1>{{ route.meta.title && t(route.meta.title) }}</h1>
  <p>{{ t('placeholder.empty') }}</p>
</template>
