<script setup lang="ts">
import { Badge, Button, Dialog } from '@asterism-dev/lumen'
import { ArrowUpRight } from '@lucide/vue'
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'

import { api } from '@/api'
import Meter from '@/components/Meter.vue'
import OpenVMDialog from '@/components/OpenVMDialog.vue'
import { gb, liveUsage, powerActions, vmTones, type VmInfo } from '@/composables/nodes'
import { useMe } from '@/composables/useMe'

const props = defineProps<{ orgId: string; vms: VmInfo[]; empty: string }>()
const emit = defineEmits<{ changed: [] }>()
const { t } = useI18n()
const router = useRouter()
const { me } = useMe()

const error = ref('')
const busy = ref(false)
const pendingDelete = ref<VmInfo | null>(null)
const opening = ref<VmInfo | null>(null)

const isMine = (v: VmInfo) => v.owner.kind === 'user' && v.owner.id === me.value?.user.id

async function act(fn: () => Promise<unknown>) {
  busy.value = true
  error.value = ''
  try {
    await fn()
    emit('changed')
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    pendingDelete.value = null
    busy.value = false
  }
}

const power = (v: VmInfo, action: string) => act(() => api(`/api/orgs/${props.orgId}/vms/${v.id}/${action}`, { method: 'POST' }))
const remove = (v: VmInfo) => act(() => api(`/api/orgs/${props.orgId}/vms/${v.id}`, { method: 'DELETE' }))
</script>

<template>
  <p v-if="error" role="alert">{{ error }}</p>
  <ul class="rows">
    <li v-for="v in vms" :key="v.id" class="vm" @click="router.push(`/orgs/${orgId}/vms/${v.id}`)">
      <span class="dot" :class="vmTones[v.state]" />
      <div class="ident">
        <router-link :to="`/orgs/${orgId}/vms/${v.id}`" class="name" @click.stop>{{ v.name }}</router-link>
        <span class="mono muted">{{ v.cpus }} vCPU · {{ gb(v.memoryMB) }} GB · {{ v.diskGB }} GB</span>
      </div>
      <span class="badges">
        <Badge variant="outline" weight="bold">{{ v.node.name }}</Badge>
        <Badge variant="outline">{{ isMine(v) ? t('vms.mine') : v.owner.name }}</Badge>
        <Badge :tone="vmTones[v.state]">{{ t(`vms.states.${v.state}`) }}</Badge>
      </span>
      <span class="meters">
        <Meter :label="t('usage.cpu')" :percent="liveUsage(v).cpu" />
        <Meter :label="t('usage.mem')" :percent="liveUsage(v).mem" />
        <Meter :label="t('usage.dsk')" :percent="liveUsage(v).dsk" />
      </span>
      <span class="actions" @click.stop>
        <template v-if="v.canPower">
          <Button v-for="a in powerActions(v.state)" :key="a" size="sm" variant="ghost" :disabled="busy" @click="power(v, a)">{{ t(`vms.${a}`) }}</Button>
        </template>
        <Button v-if="v.canDelete" size="sm" variant="ghost" :disabled="busy" @click="pendingDelete = v">{{ t('vms.delete') }}</Button>
        <Button size="sm" :icon="ArrowUpRight" @click="opening = v">{{ t('nodes.open') }}</Button>
      </span>
    </li>
    <li v-if="!vms.length" class="empty">{{ empty }}</li>
  </ul>
  <OpenVMDialog :open="!!opening" :org-id="orgId" :vm="opening" @update:open="(o) => !o && (opening = null)" />
  <Dialog
    :open="!!pendingDelete"
    :title="t('vms.deleteTitle')"
    :description="t('vms.deleteBody', { name: pendingDelete?.name })"
    @update:open="(o) => !o && !busy && (pendingDelete = null)"
  >
    <template #actions>
      <Button :disabled="busy" @click="pendingDelete = null">{{ t('profile.cancel') }}</Button>
      <Button variant="danger" :disabled="busy" @click="pendingDelete && remove(pendingDelete)">{{ t('vms.delete') }}</Button>
    </template>
  </Dialog>
</template>

<style scoped>
.rows {
  list-style: none;
  margin: 0;
  padding: 0;
  display: grid;
  gap: 8px;
}
.vm {
  display: grid;
  grid-template-columns: auto minmax(140px, 1.2fr) auto 2fr auto;
  align-items: center;
  gap: 16px;
  padding: 12px 16px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--panel);
  cursor: pointer;
}
.vm:hover {
  border-color: var(--accent);
}
.dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--muted);
}
.dot.success {
  background: var(--success);
}
.dot.warning {
  background: var(--warning);
}
.dot.danger {
  background: var(--danger);
}
.ident {
  display: flex;
  flex-direction: column;
  min-width: 0;
}
.name {
  color: var(--text);
  font-family: var(--font-mono, ui-monospace, monospace);
  font-weight: 600;
  text-decoration: none;
}
.mono {
  font-family: var(--font-mono, ui-monospace, monospace);
  font-size: 0.75rem;
}
.muted {
  color: var(--muted);
}
.badges,
.meters,
.actions {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px 16px;
}
.badges {
  gap: 6px;
}
.actions {
  gap: 4px;
  justify-content: flex-end;
}
.empty {
  padding: 24px;
  text-align: center;
  color: var(--muted);
  border: 1px dashed var(--border);
  border-radius: var(--radius);
}
@media (max-width: 900px) {
  .vm {
    grid-template-columns: auto 1fr;
  }
  .badges,
  .meters,
  .actions {
    grid-column: 2;
    justify-content: flex-start;
  }
}
</style>
