<script setup lang="ts">
import { Checkbox, NumberInput } from '@asterism-dev/lumen'
import { reactive, watch } from 'vue'
import { useI18n } from 'vue-i18n'

export interface Limits {
  vcpu: number | null
  ramMb: number | null
  diskGb: number | null
  vmCount: number | null
  snapshotCount: number | null
}

const model = defineModel<Limits>({ required: true })
const valid = defineModel<boolean>('valid', { default: true })
defineProps<{ disabled?: boolean }>()
const { t } = useI18n()

const fields = ['vcpu', 'ramMb', 'diskGb', 'vmCount', 'snapshotCount'] as const

// Unlimited is driven only by the checkbox; an empty input is "no value yet", not unlimited.
const unlimited = reactive(Object.fromEntries(fields.map((k) => [k, model.value[k] === null])) as Record<keyof Limits, boolean>)
const raw = reactive({ ...model.value })

const isValid = (v: number | null) => v !== null && Number.isInteger(v) && v >= 0
const problem = (k: keyof Limits) => {
  if (unlimited[k]) return ''
  if (raw[k] === null) return t('limits.required')
  return isValid(raw[k]) ? '' : t('limits.invalid')
}
const emitted = (k: keyof Limits) => (unlimited[k] ? null : isValid(raw[k]) ? raw[k] : model.value[k])

function update() {
  model.value = Object.fromEntries(fields.map((k) => [k, emitted(k)])) as unknown as Limits
  valid.value = fields.every((k) => !problem(k))
}

const setValue = (k: keyof Limits, v: number | null) => {
  raw[k] = v
  update()
}
const setUnlimited = (k: keyof Limits, u: boolean) => {
  unlimited[k] = u
  if (!u && raw[k] === null) raw[k] = 0
  update()
}

// Parent replaced the limits (e.g. after a refresh).
watch(model, (m) => {
  for (const k of fields) {
    if (emitted(k) !== m[k]) {
      unlimited[k] = m[k] === null
      raw[k] = m[k]
    }
  }
  valid.value = fields.every((k) => !problem(k))
})
</script>

<template>
  <div class="limits">
    <div v-for="k in fields" :key="k" class="limits__field">
      <label>
        {{ t(`limits.${k}`) }}
        <NumberInput
          :model-value="unlimited[k] ? null : raw[k]"
          :disabled="disabled || unlimited[k]"
          :placeholder="t('limits.unlimited')"
          :invalid="!!problem(k)"
          :aria-label="t(`limits.${k}`)"
          @update:model-value="(v) => setValue(k, v)"
        />
      </label>
      <Checkbox :model-value="unlimited[k]" :disabled="disabled" @update:model-value="(u) => setUnlimited(k, u === true)">
        {{ t('limits.unlimited') }}
      </Checkbox>
      <small v-if="!disabled && problem(k)" role="alert">{{ problem(k) }}</small>
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
