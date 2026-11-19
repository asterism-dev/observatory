<script setup lang="ts">
import { List, ListItem } from '@asterism-dev/lumen'
import { Box, HardDrive, KeyRound, Laptop, LayoutDashboard, Network, Server } from '@lucide/vue'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'

import { ApiError } from '@/api'
import CreateOrgDialog from '@/components/CreateOrgDialog.vue'
import UserMenu from '@/components/UserMenu.vue'
import { useMe, type Org } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const { me, activeOrg, selectOrg, load: loadMe } = useMe()
const { load: loadOrg } = useOrg()
const creating = ref(false)

// Admin routes reuse :orgId for orgs the admin need not belong to.
const orgId = computed(() => (route.path.startsWith('/admin/') ? '' : ((route.params.orgId as string | undefined) ?? '')))

const orgError = ref('')

// Without an org in the route (e.g. /profile) load the active org so the user menu knows its permissions.
watch(
  () => orgId.value || (route.path.startsWith('/admin/') ? '' : (activeOrg.value?.id ?? '')),
  async (id) => {
    orgError.value = ''
    if (!id) return
    if (me.value?.orgs.some((o) => o.id === id)) selectOrg(id)
    try {
      await loadOrg(id)
    } catch (e) {
      if (id !== (orgId.value || activeOrg.value?.id)) return
      if (e instanceof ApiError && (e.status === 403 || e.status === 404)) {
        // Refresh the org list first so the guard's '/' redirect doesn't land on this org again.
        const target = await loadMe().then(() => '/', () => '/profile')
        if (id === (orgId.value || activeOrg.value?.id)) router.replace(target)
      } else orgError.value = e instanceof Error ? e.message : String(e)
    }
  },
  { immediate: true },
)

function switchOrg(id: string) {
  selectOrg(id)
  router.push(orgId.value ? route.path.replace(orgId.value, id) : `/orgs/${id}/dashboard`)
}

async function onCreated(org: Org) {
  await loadMe()
  switchOrg(org.id)
}

const navItems = computed(() => {
  const id = activeOrg.value?.id
  if (!id) return []
  return [
    { path: `/orgs/${id}/dashboard`, label: t('nav.dashboard'), icon: LayoutDashboard },
    { path: `/orgs/${id}/nodes`, label: t('nav.nodes'), icon: Server },
    { path: `/orgs/${id}/vms`, label: t('nav.vms'), icon: Box },
    { path: `/orgs/${id}/disks`, label: t('nav.disks'), icon: HardDrive },
    { path: `/orgs/${id}/networks`, label: t('nav.networks'), icon: Network },
    { path: `/orgs/${id}/devices`, label: t('nav.devices'), icon: Laptop },
    { path: `/orgs/${id}/env`, label: t('nav.env'), icon: KeyRound },
  ]
})

const activePath = computed(() => navItems.value.find((i) => route.path === i.path || route.path.startsWith(i.path + '/'))?.path ?? route.path)
</script>

<template>
  <div class="shell">
    <aside class="side">
      <div class="side__head">{{ t('app.name') }}</div>
      <List :model-value="activePath" :aria-label="t('nav.label')" @update:model-value="(p) => p && router.push(p)">
        <ListItem v-for="item in navItems" :key="item.path" :value="item.path">
          <span class="nav-item"><component :is="item.icon" :size="16" :stroke-width="1.75" />{{ item.label }}</span>
        </ListItem>
      </List>
      <div class="side__spacer" />
      <UserMenu @switch="switchOrg" @create="creating = true" />
    </aside>
    <main class="content">
      <p v-if="orgError" role="alert">{{ orgError }}</p>
      <slot />
    </main>
    <CreateOrgDialog v-model:open="creating" @created="onCreated" />
  </div>
</template>

<style scoped>
.shell {
  display: flex;
  height: 100%;
}
.side {
  display: flex;
  flex-direction: column;
  gap: 8px;
  width: 240px;
  padding: 12px;
  border-right: 1px solid var(--border);
  background: var(--panel);
}
.side__head {
  padding: 4px 8px 8px;
  font-weight: 600;
}
.side__spacer {
  flex: 1;
}
.nav-item {
  display: flex;
  align-items: center;
  gap: 8px;
}
.content {
  flex: 1;
  min-width: 0;
  padding: 16px;
  overflow: auto;
}
@media (max-width: 767.98px) {
  .shell {
    flex-direction: column;
  }
  .side {
    width: auto;
    border-right: 0;
    border-bottom: 1px solid var(--border);
  }
  .side__spacer {
    display: none;
  }
}
</style>
