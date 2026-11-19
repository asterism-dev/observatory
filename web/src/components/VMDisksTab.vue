<script setup lang="ts">
import { Button } from '@asterism-dev/lumen'
import { useI18n } from 'vue-i18n'

import type { VmInfo } from '@/composables/nodes'
import { Card, KeyValue, KeyValueList, Section, Tag } from '@/ui'

defineProps<{ vm: VmInfo; busy: boolean }>()
const emit = defineEmits<{ edit: [] }>()
const { t } = useI18n()
</script>

<template>
  <Section :label="t('vmDisks.section')">
    <template #actions>
      <Button v-if="vm.canDisks" size="sm" :disabled="busy" @click="emit('edit')">{{ t('vmDisks.change') }}</Button>
    </template>
    <Card>
      <KeyValueList>
        <KeyValue :label="t('vmDisks.root')"><code>/</code> <span class="muted">{{ vm.diskGB }} GB</span></KeyValue>
        <KeyValue v-for="dk in vm.disks" :key="dk.id" :label="dk.name">
          <code>{{ dk.mountPath }}</code> <span class="muted">{{ dk.sizeGB }} GB</span>
          <Tag :tone="dk.error ? 'danger' : 'muted'">{{ t(`disks.states.${dk.state}`) }}</Tag>
          <small v-if="dk.error" role="alert">{{ dk.error }}</small>
        </KeyValue>
      </KeyValueList>
    </Card>
  </Section>
</template>
