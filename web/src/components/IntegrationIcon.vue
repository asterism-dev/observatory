<script setup lang="ts">
import type { IntegrationKind } from '@/composables/integrations'

withDefaults(defineProps<{ kind: IntegrationKind; size?: number }>(), { size: 20 })
</script>

<template>
  <svg class="integration-icon" :width="size" :height="size" :viewBox="kind === 'github' ? '0 0 16 16' : '0 0 24 24'" aria-hidden="true" focusable="false">
    <path
      v-if="kind === 'github'"
      fill="currentColor"
      d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z"
    />
    <path
      v-else-if="kind === 'claude'"
      fill="none"
      stroke="#D97757"
      stroke-width="2.2"
      stroke-linecap="round"
      d="M14.51 12.67L21.66 14.59M13.84 13.84L17.80 17.80M12.67 14.51L14.59 21.66M11.33 14.51L9.88 19.92M10.16 13.84L4.93 19.07M9.49 12.67L4.08 14.12M9.49 11.33L2.34 9.41M10.16 10.16L6.20 6.20M11.33 9.49L9.41 2.34M12.67 9.49L14.12 4.08M13.84 10.16L19.07 4.93M14.51 11.33L19.92 9.88"
    />
    <path
      v-else-if="kind === 'gcloud'"
      fill="none"
      stroke="currentColor"
      stroke-width="1.8"
      stroke-linecap="round"
      stroke-linejoin="round"
      d="M7 18a4.5 4.5 0 0 1-.6-8.96A6 6 0 0 1 18 8.5a4.75 4.75 0 0 1-.5 9.5Z"
    />
    <g v-else fill="#5E6AD2" stroke="#5E6AD2">
      <path stroke="none" d="M3.55 6.66A10 10 0 1 1 17.34 20.45Z" />
      <path fill="none" stroke-width="1.3" stroke-linecap="round" d="M2.86 9.08L14.92 21.14M2.41 11.60L12.40 21.59M2.77 14.65L9.35 21.23" />
    </g>
  </svg>
</template>

<style scoped>
.integration-icon {
  flex: none;
  vertical-align: -0.2em;
}
</style>
