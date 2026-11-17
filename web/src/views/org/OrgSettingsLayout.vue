<script setup lang="ts">
import { useI18n } from 'vue-i18n'

import SettingsLayout from '@/components/SettingsLayout.vue'
import { useSettingsNav } from '@/composables/useSettingsNav'

const { t } = useI18n()
const { orgTabs } = useSettingsNav()
</script>

<template>
  <SettingsLayout :title="t('nav.orgSettings')" :tabs="orgTabs" />
</template>
