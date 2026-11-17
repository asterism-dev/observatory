<script setup lang="ts">
import { Button, List, ListItem, Menu, WorkspaceRail, type MenuItem } from '@asterism-dev/lumen'
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'

import { api } from '@/api'
import { useMe } from '@/composables/useMe'
import { THEME_PREFERENCES, useTheme } from '@/composables/useTheme'
import { LOCALES } from '@/i18n'

const { t, locale } = useI18n()
const route = useRoute()
const router = useRouter()
const { me, activeOrg, selectOrg } = useMe()
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

const navItems = computed(() => [
  { path: '/profile', label: t('nav.profile') },
  ...(me.value?.user.platformAdmin ? [{ path: '/admin/audit', label: t('nav.audit') }] : []),
])

async function signOut() {
  await api('/auth/logout', { method: 'POST' })
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
    <WorkspaceRail :workspaces="workspaces" :active="activeOrg?.id ?? null" @select="selectOrg" />
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
    <main class="content"><slot /></main>
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
