<script setup lang="ts">
import { useI18n } from 'vue-i18n'

const { t } = useI18n()
</script>

<template>
  <p>{{ t('integrations.gcloudHint') }}</p>
  <pre><code>gcloud auth application-default login
observatory integration connect gcloud</code></pre>
</template>
