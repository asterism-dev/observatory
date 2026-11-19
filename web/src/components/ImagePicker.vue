<script setup lang="ts">
import { Select } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import type { Image } from '@/composables/images'

const model = defineModel<string>({ required: true })
const props = defineProps<{ orgId: string; defaultLabel?: string; disabled?: boolean }>()

const { t } = useI18n()
const images = ref<Image[]>([])

// Images without versions cannot be used yet; latest is the bare reference.
const options = computed(() => [
  ...(props.defaultLabel !== undefined ? [{ value: '', label: props.defaultLabel }] : []),
  ...images.value.flatMap((img) =>
    img.platform
      ? [{ value: img.ref, label: img.ref }]
      : Object.keys(img.tags)
          .sort((a, b) => (a === 'latest' ? -1 : b === 'latest' ? 1 : a.localeCompare(b)))
          .map((tag) => (tag === 'latest' ? { value: img.ref, label: `${img.ref}:latest` } : { value: `${img.ref}:${tag}`, label: `${img.ref}:${tag}` })),
  ),
])

watch(
  () => props.orgId,
  async (id) => {
    images.value = await api<Image[]>(`/api/orgs/${id}/images`).catch(() => [])
  },
  { immediate: true },
)
</script>

<template>
  <Select v-model="model" :options="options" :disabled="disabled" :placeholder="t('images.pick')" :aria-label="t('images.image')" />
</template>
