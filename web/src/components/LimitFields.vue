<script setup lang="ts">
import { Checkbox, NumberInput } from '@asterism-dev/lumen'
import { useI18n } from 'vue-i18n'

export interface Limits {
  vcpu: number | null
  ramMb: number | null
  diskGb: number | null
  vmCount: number | null
}

const model = defineModel<Limits>({ required: true })
defineProps<{ disabled?: boolean }>()
const { t } = useI18n()

const fields = ['vcpu', 'ramMb', 'diskGb', 'vmCount'] as const

const set = (k: keyof Limits, v: number | null) => (model.value = { ...model.value, [k]: v })
</script>

<template>
  <div class="limits">
    <div v-for="k in fields" :key="k" class="limits__field">
      <label>
        {{ t(`limits.${k}`) }}
        <NumberInput
          :model-value="model[k]"
          :disabled="disabled || model[k] === null"
          :placeholder="t('limits.unlimited')"
          :aria-label="t(`limits.${k}`)"
          @update:model-value="(v) => set(k, v)"
        />
      </label>
      <Checkbox
        :model-value="model[k] === null"
        :disabled="disabled"
        @update:model-value="(u) => set(k, u === true ? null : 0)"
      >
        {{ t('limits.unlimited') }}
      </Checkbox>
    </div>
  </div>
</template>

<style scoped>
.limits {
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
}
.limits__field {
  display: flex;
  flex-direction: column;
  gap: 4px;
  width: 140px;
}
</style>
