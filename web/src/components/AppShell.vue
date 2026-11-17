<script setup lang="ts">
import { Button, List, ListItem, Menu, WorkspaceRail, type MenuItem } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'

import { api, ApiError } from '@/api'
import CreateOrgDialog from '@/components/CreateOrgDialog.vue'
import { useMe, type Org } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'
import { THEME_PREFERENCES, useTheme } from '@/composables/useTheme'
import { LOCALES } from '@/i18n'

const { t, locale } = useI18n()
const route = useRoute()
const router = useRouter()
const { me, activeOrg, selectOrg, load: loadMe } = useMe()
const { can, load: loadOrg } = useOrg()
const creating = ref(false)
const { theme } = useTheme()

const initials = (name: string) =>
  name
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((w) => w[0]!.toUpperCase())
    .join('')

const workspaces = computed(() =>
  (me.value?.orgs ?? []).map((o) => ({ id: o.id, name: o.name, label: initials(o.name) })),
)

const orgId = computed(() => (route.params.orgId as string | undefined) ?? '')

const orgError = ref('')

watch(
  orgId,
  async (id) => {
    orgError.value = ''
    if (!id) return
    if (me.value?.orgs.some((o) => o.id === id)) selectOrg(id)
    try {
      await loadOrg(id)
    } catch (e) {
      if (id !== orgId.value) return
      if (e instanceof ApiError && (e.status === 403 || e.status === 404)) {
        // Refresh the org list first so the guard's '/' redirect doesn't land on this org again.
        const target = await loadMe().then(() => '/', () => '/profile')
        if (id === orgId.value) router.replace(target)
      } else orgError.value = e instanceof Error ? e.message : String(e)
    }
  },
  { immediate: true },
)

function switchOrg(id: string) {
  selectOrg(id)
  router.push(orgId.value ? route.path.replace(orgId.value, id) : `/orgs/${id}/members`)
}

async function onCreated(org: Org) {
  await loadMe()
  switchOrg(org.id)
}

const orgNav = computed(() =>
  orgId.value
    ? [
        { action: 'member:read', path: `/orgs/${orgId.value}/members`, label: t('nav.members') },
        { action: 'org:update', path: `/orgs/${orgId.value}/settings`, label: t('nav.settings') },
      ].filter((i) => can(i.action))
    : [],
)

const navItems = computed(() => [
  ...orgNav.value,
  { path: '/profile', label: t('nav.profile') },
  ...(me.value?.user.platformAdmin ? [{ path: '/admin/audit', label: t('nav.audit') }] : []),
])

async function signOut() {
  try {
    await api('/auth/logout', { method: 'POST' })
  } catch (e) {
    // 401: the session is already revoked, which is what signing out wants anyway.
    if (!(e instanceof ApiError && e.status === 401)) {
      console.error('sign out failed', e)
      return
    }
  }
  me.value = null
  await router.push('/login')
}

const userMenu = (): MenuItem[] => [
  ...LOCALES.map((l) => ({
    label: l.name,
    checked: locale.value === l.code,
    action: () => (locale.value = l.code),
  })),
  ...THEME_PREFERENCES.map((p) => ({
    label: t(`theme.${p}`),
    checked: theme.value === p,
    action: () => (theme.value = p),
  })),
  { label: t('user.signOut'), danger: true, action: signOut },
]
</script>

<template>
  <div class="shell">
    <WorkspaceRail :workspaces="workspaces" :active="activeOrg?.id ?? null" @select="switchOrg" @add="creating = true" />
    <aside class="side">
      <div class="side__head">
        <strong>{{ activeOrg?.name }}</strong>
        <Menu :items="userMenu()" align="end">
          <Button variant="ghost" size="sm">{{ me?.user.name || me?.user.email }}</Button>
        </Menu>
      </div>
      <List :model-value="route.path" :aria-label="t('nav.label')" @update:model-value="(p) => p && router.push(p)">
        <ListItem v-for="item in navItems" :key="item.path" :value="item.path">{{ item.label }}</ListItem>
      </List>
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
  width: 220px;
  padding: 12px;
  border-right: 1px solid var(--border);
  background: var(--panel);
}
.side__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
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
    flex-direction: column-reverse;
  }
  .side {
    width: auto;
    border-right: 0;
    border-bottom: 1px solid var(--border);
  }
}
</style>
