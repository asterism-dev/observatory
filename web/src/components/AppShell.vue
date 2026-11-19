<script setup lang="ts">
import { List, ListItem } from '@asterism-dev/lumen'
import { BookOpen, Box, HardDrive, KeyRound, Laptop, LayoutDashboard, Network, Plug, Server } from '@lucide/vue'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'

import { ApiError } from '@/api'
import CreateOrgDialog from '@/components/CreateOrgDialog.vue'
import UserMenu from '@/components/UserMenu.vue'
import { useCrumbs, type Crumb } from '@/composables/useCrumbs'
import { useMe, type Org } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'
import { useSettingsNav } from '@/composables/useSettingsNav'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const { me, activeOrg, selectOrg, load: loadMe } = useMe()
const { org, load: loadOrg } = useOrg()
const { orgTabs, platformTabs } = useSettingsNav()
const { extra } = useCrumbs()
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
    { path: `/orgs/${id}/integrations`, label: t('nav.integrations'), icon: Plug },
  ]
})

const crumbs = computed<Crumb[]>(() => {
  const own = extra.value.path === route.path ? extra.value.crumbs : []
  const under = (p: string) => route.path === p || route.path.startsWith(p + '/')
  if (route.path === '/profile') return [{ label: t('nav.profile') }]
  if (route.path.startsWith('/admin/')) {
    const tab = platformTabs.value.find((i) => under(i.path))
    return [{ label: t('nav.platformSettings') }, ...(tab ? [{ label: tab.label, to: tab.path }] : []), ...own]
  }
  if (!orgId.value || org.value?.id !== orgId.value) return []
  const out: Crumb[] = [{ label: org.value.name, to: `/orgs/${orgId.value}/dashboard` }]
  const item = navItems.value.find((i) => under(i.path))
  const tab = orgTabs.value.find((i) => under(i.path))
  if (item) out.push({ label: item.label, to: item.path })
  else if (tab) out.push({ label: t('nav.orgSettings'), to: `/orgs/${orgId.value}/settings` }, { label: tab.label, to: tab.path })
  return [...out, ...own]
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
      <header class="topbar">
        <nav class="crumbs" :aria-label="t('nav.breadcrumb')">
          <template v-for="(c, i) in crumbs" :key="i">
            <span v-if="i" class="sep">/</span>
            <router-link v-if="c.to && i < crumbs.length - 1" :to="c.to" :class="{ org: i === 0 && orgId }">{{ c.label }}</router-link>
            <span v-else :class="{ org: i === 0 && orgId, current: i === crumbs.length - 1 }" :aria-current="i === crumbs.length - 1 ? 'page' : undefined">{{ c.label }}</span>
          </template>
        </nav>
        <a v-if="me?.docsURL" :href="me.docsURL" target="_blank" rel="noopener" class="docs"><BookOpen :size="16" :stroke-width="1.75" />{{ t('nav.docs') }}</a>
      </header>
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
.topbar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 16px;
  margin: -16px -16px 16px;
  padding: 12px 16px;
  border-bottom: 1px solid var(--border);
}
.crumbs {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: 4px 10px;
  min-width: 0;
}
.crumbs a {
  color: var(--muted);
  text-decoration: none;
}
.crumbs a:hover {
  color: var(--text);
}
.org {
  font-family: var(--font-mono, ui-monospace, monospace);
  font-size: 0.7rem;
  letter-spacing: 0.08em;
  text-transform: uppercase;
}
.sep {
  color: var(--muted);
  opacity: 0.6;
}
.current {
  color: var(--text);
}
.docs {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  color: var(--text);
  text-decoration: none;
}
.docs:hover {
  color: var(--accent);
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
