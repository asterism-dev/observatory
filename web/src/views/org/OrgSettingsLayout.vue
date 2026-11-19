<script setup lang="ts">
import { Building2 } from '@lucide/vue'
import { useI18n } from 'vue-i18n'

import SettingsLayout from '@/components/SettingsLayout.vue'
import { useSettingsNav } from '@/composables/useSettingsNav'

const { t } = useI18n()
const { orgTabs } = useSettingsNav()
</script>

<template>
  <SettingsLayout :icon="Building2" :title="t('nav.orgSettings')" :subtitle="t('nav.orgSettingsSubtitle')" :tabs="orgTabs" />
</template>
