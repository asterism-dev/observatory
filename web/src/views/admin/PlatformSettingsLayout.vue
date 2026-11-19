<script setup lang="ts">
import { Shield } from '@lucide/vue'
import { useI18n } from 'vue-i18n'

import SettingsLayout from '@/components/SettingsLayout.vue'
import { useSettingsNav } from '@/composables/useSettingsNav'

const { t } = useI18n()
const { platformTabs } = useSettingsNav()
</script>

<template>
  <SettingsLayout :icon="Shield" :title="t('nav.platformSettings')" :subtitle="t('nav.platformSettingsSubtitle')" :tabs="platformTabs" />
</template>
