<script setup lang="ts">
import { useI18n } from 'vue-i18n'

import SettingsLayout from '@/components/SettingsLayout.vue'
import { useSettingsNav } from '@/composables/useSettingsNav'

const { t } = useI18n()
const { platformTabs } = useSettingsNav()
</script>

<template>
  <SettingsLayout :title="t('nav.platformSettings')" :tabs="platformTabs" />
</template>
