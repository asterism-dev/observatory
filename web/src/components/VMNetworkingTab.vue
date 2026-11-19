<script setup lang="ts">
import { Button } from '@asterism-dev/lumen'
import { useI18n } from 'vue-i18n'

import type { VmInfo } from '@/composables/nodes'
import { Card, KeyValue, KeyValueList, Section, Tag } from '@/ui'

defineProps<{ vm: VmInfo; egress: { secrets: number; hosts: string[]; caNotAfter: string | null; trustChanged: boolean } | null; busy: boolean }>()
const emit = defineEmits<{ edit: [] }>()
const { t, d } = useI18n()
</script>

<template>
    <div class="split">
      <Section :label="t('vms.connect')">
        <Card>
          <KeyValueList>
            <KeyValue :label="t('vms.ip')">{{ vm.ip || '–' }} <small v-if="vm.ip" class="muted">({{ t('vms.ipHint') }})</small></KeyValue>
            <KeyValue :label="t('networks.mgmt')">{{ vm.mgmtIP || '–' }}</KeyValue>
            <KeyValue :label="t('networks.dns')">{{ vm.dnsName || '–' }}</KeyValue>
          </KeyValueList>
        </Card>
      </Section>
      <Section v-if="egress" :label="t('vms.egress')">
        <Card>
          <KeyValueList>
            <KeyValue :label="t('vms.egress')">{{ t('vms.egressSecrets', { n: egress.secrets }) }}</KeyValue>
            <KeyValue v-if="egress.hosts.length" :label="t('vms.egressHosts')"><Tag v-for="h in egress.hosts" :key="h">{{ h }}</Tag></KeyValue>
            <KeyValue v-if="egress.caNotAfter" :label="t('vms.egressCaLabel')">{{ t('vms.egressCa', { date: d(new Date(egress.caNotAfter), { dateStyle: 'short' }) }) }}</KeyValue>
          </KeyValueList>
          <small v-if="egress.trustChanged" role="status" class="muted">{{ t('vms.egressRestart') }}</small>
        </Card>
      </Section>
    </div>
    <Section :label="t('networks.section')">
      <template #actions>
        <Button v-if="vm.canNetwork" size="sm" :disabled="busy" @click="emit('edit')">{{ t('networks.change') }}</Button>
      </template>
      <Card>
        <KeyValueList v-if="vm.networks.length">
          <KeyValue v-for="n in vm.networks" :key="n.id" :label="n.name"><code>{{ n.ip }}</code> <span class="muted">{{ n.dnsName }}</span></KeyValue>
        </KeyValueList>
        <span v-else class="muted">{{ t('networks.none') }}</span>
      </Card>
    </Section>
</template>

<style scoped>
.split {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(300px, 1fr));
  align-items: start;
  gap: var(--section-gap) 16px;
}
</style>
