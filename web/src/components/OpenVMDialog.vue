<script setup lang="ts">
import { Badge, Button, Dialog } from '@asterism-dev/lumen'
import { Code, Copy, Zap } from '@lucide/vue'
import QRCode from 'qrcode'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import type { VmInfo } from '@/composables/nodes'

const props = defineProps<{ orgId: string; vm: VmInfo | null }>()
const open = defineModel<boolean>('open', { required: true })
const { t } = useI18n()

const host = computed(() => props.vm?.dnsName ?? '')
const commands = computed(() => (props.vm ? [`observatory ssh ${props.vm.name}`, `observatory desktop ${props.vm.name}`] : []))
const editors = computed(() => [
  { name: 'VS Code', icon: Code, href: `vscode://vscode-remote/ssh-remote+ubuntu@${host.value}/home/ubuntu` },
  { name: 'Zed', icon: Zap, href: `zed://ssh/ubuntu@${host.value}/home/ubuntu` },
])
const qr = ref('')
const copied = ref('')
const copyFailed = ref(false)

async function copy(text: string) {
  copyFailed.value = false
  try {
    await navigator.clipboard.writeText(text)
    copied.value = text
  } catch {
    copyFailed.value = true
  }
}

watch(
  () => [open.value, host.value],
  async () => {
    copied.value = ''
    copyFailed.value = false
    qr.value = ''
    if (!open.value || !host.value) return
    const want = host.value
    const svg = await QRCode.toString(`ssh://ubuntu@${want}`, { type: 'svg', margin: 1 }).catch(() => '')
    if (want === host.value) qr.value = svg
  },
  { immediate: true },
)
</script>

<template>
  <Dialog v-model:open="open" :title="vm ? t('open.title', { name: vm.name }) : ''" :description="t('open.subtitle')" width="640px">
    <template v-if="vm">
      <p><Badge variant="outline" weight="bold">{{ vm.node.name }}</Badge></p>
      <section>
        <div class="label"><span>{{ t('open.cli') }}</span><span>{{ t('open.cliHint') }}</span></div>
        <div v-for="c in commands" :key="c" class="cmd">
          <code>$ {{ c }}</code>
          <Button size="sm" :icon="Copy" @click="copy(c)">{{ copied === c ? t('open.copied') : t('networks.copy') }}</Button>
        </div>
        <small>
          {{ t('open.desktopHint') }}
          <a href="https://github.com/Xpra-org/xpra/wiki/Download" target="_blank" rel="noopener">{{ t('vms.desktopInstall') }}</a>
        </small>
      </section>
      <section>
        <div class="label">
          <span>{{ t('open.editors') }}</span>
          <span>{{ t('open.overSsh') }} <code>{{ host }}</code>
            <button type="button" class="icon-btn" :aria-label="t('networks.copy')" @click="copy(host)"><Copy :size="12" /></button></span>
        </div>
        <div class="editors">
          <a v-for="e in editors" :key="e.name" :href="e.href" class="tile"><component :is="e.icon" :size="20" :stroke-width="1.75" />{{ e.name }}</a>
        </div>
      </section>
      <section>
        <div class="label"><span>{{ t('open.mobile') }}</span></div>
        <div class="mobile">
          <div v-if="qr" class="qr" role="img" :aria-label="t('open.scan')" v-html="qr" />
          <div>
            <strong>{{ t('open.scan') }}</strong>
            <i18n-t keypath="open.scanHint" tag="p">
              <template #name>{{ vm.name }}</template>
              <template #devices><router-link :to="`/orgs/${orgId}/devices`" @click="open = false">{{ t('nav.devices') }}</router-link></template>
            </i18n-t>
          </div>
        </div>
      </section>
      <p v-if="copyFailed" role="alert">{{ t('networks.copyFailed') }}</p>
    </template>
    <template #actions><Button @click="open = false">{{ t('nodes.done') }}</Button></template>
  </Dialog>
</template>

<style scoped>
section {
  margin-top: 16px;
}
.label {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
  font-family: var(--font-mono, ui-monospace, monospace);
  font-size: 0.7rem;
  color: var(--muted);
}
.label > span:first-child {
  letter-spacing: 0.08em;
  text-transform: uppercase;
}
.cmd {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 8px;
  padding: 8px 8px 8px 14px;
  margin-bottom: 6px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
}
.icon-btn {
  padding: 0 2px;
  border: 0;
  background: none;
  color: inherit;
  cursor: pointer;
}
.editors {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(120px, 1fr));
  gap: 8px;
}
.tile {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  padding: 16px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  color: var(--text);
  text-decoration: none;
  font-weight: 600;
}
.tile:hover {
  border-color: var(--accent);
}
.mobile {
  display: flex;
  align-items: center;
  gap: 16px;
  padding: 14px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
}
.mobile p {
  margin: 4px 0 0;
  color: var(--muted);
}
.qr {
  flex: none;
  width: 110px;
  padding: 6px;
  border-radius: var(--radius);
  background: #fff;
}
.qr :deep(svg) {
  display: block;
  width: 100%;
  height: auto;
}
</style>
