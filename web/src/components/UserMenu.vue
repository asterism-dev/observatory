<script setup lang="ts">
import { Popover, SegmentedControl } from '@asterism-dev/lumen'
import { Building2, Check, ChevronUp, LogOut, Monitor, Moon, Plus, Shield, Sun, User } from '@lucide/vue'
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'

import { api, ApiError } from '@/api'
import { useMe } from '@/composables/useMe'
import { useSettingsNav } from '@/composables/useSettingsNav'
import { THEME_PREFERENCES, useTheme } from '@/composables/useTheme'
import { LOCALES } from '@/i18n'

const emit = defineEmits<{ switch: [orgId: string]; create: [] }>()

const { t, locale } = useI18n()
const router = useRouter()
const { me, activeOrg } = useMe()
const { orgTabs, platformTabs } = useSettingsNav()
const open = ref(false)
const { theme } = useTheme()

const THEME_ICONS = { system: Monitor, light: Sun, dark: Moon }
const localeOptions = LOCALES.map((l) => ({ value: l.code, label: l.code.toUpperCase(), ariaLabel: l.name }))
const themeOptions = computed(() => THEME_PREFERENCES.map((p) => ({ value: p, icon: THEME_ICONS[p], ariaLabel: t(`theme.${p}`) })))

const initial = (name: string) => name.trim()[0]?.toUpperCase() ?? '?'

function pick(fn: () => void) {
  open.value = false
  fn()
}

async function signOut() {
  open.value = false
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
</script>

<template>
  <Popover v-model:open="open" align="start" :aria-label="t('nav.userMenu')">
    <button type="button" class="card">
      <span class="avatar">{{ initial(me?.user.name || me?.user.email || '') }}</span>
      <span class="card__text">
        <span class="card__name">{{ me?.user.name || me?.user.email }}</span>
        <span class="card__org">{{ activeOrg?.name }}</span>
      </span>
      <ChevronUp :size="16" :stroke-width="1.75" />
    </button>
    <template #content>
      <div class="menu">
        <button v-for="org in me?.orgs ?? []" :key="org.id" type="button" class="row" @click="pick(() => emit('switch', org.id))">
          <span class="avatar avatar--sm">{{ initial(org.name) }}</span>
          <span class="row__label">{{ org.name }}</span>
          <Check v-if="org.id === activeOrg?.id" class="row__check" :size="16" :stroke-width="1.75" />
        </button>
        <button type="button" class="row" @click="pick(() => emit('create'))">
          <Plus :size="16" :stroke-width="1.75" />{{ t('nav.newOrg') }}
        </button>
        <hr />
        <RouterLink v-if="orgTabs.length" :to="orgTabs[0]!.path" class="row" @click="open = false">
          <Building2 :size="16" :stroke-width="1.75" />{{ t('nav.orgSettings') }}
        </RouterLink>
        <RouterLink v-if="platformTabs.length" :to="platformTabs[0]!.path" class="row" @click="open = false">
          <Shield :size="16" :stroke-width="1.75" />{{ t('nav.platformSettings') }}
        </RouterLink>
        <RouterLink to="/profile" class="row" @click="open = false">
          <User :size="16" :stroke-width="1.75" />{{ t('nav.profile') }}
        </RouterLink>
        <div class="prefs">
          <SegmentedControl v-model="locale" :options="localeOptions" size="sm" :aria-label="t('profile.language')" />
          <SegmentedControl v-model="theme" :options="themeOptions" size="sm" :aria-label="t('profile.theme')" />
        </div>
        <hr />
        <button type="button" class="row" @click="signOut">
          <LogOut :size="16" :stroke-width="1.75" />{{ t('user.signOut') }}
        </button>
      </div>
    </template>
  </Popover>
</template>

<style scoped>
.card {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
  padding: 8px;
  border: 0;
  border-radius: var(--radius);
  background: none;
  color: var(--text);
  font: inherit;
  text-align: left;
  cursor: pointer;
}
.card:hover,
.card:focus-visible {
  background: var(--select);
}
.card__text {
  display: flex;
  flex: 1;
  flex-direction: column;
  min-width: 0;
}
.card__name,
.card__org,
.row__label {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.card__name {
  font-weight: 600;
}
.card__org {
  color: var(--muted);
  font-size: 11px;
  text-transform: uppercase;
  letter-spacing: 0.04em;
}
.avatar {
  display: grid;
  flex: none;
  place-items: center;
  width: 28px;
  height: 28px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  font-size: 12px;
}
.avatar--sm {
  width: 20px;
  height: 20px;
  font-size: 11px;
}
.menu {
  display: flex;
  flex-direction: column;
  min-width: 240px;
  max-height: var(--reka-popover-content-available-height);
  overflow-y: auto;
}
.row {
  display: flex;
  align-items: center;
  gap: 8px;
  min-height: 32px;
  padding: 0 8px;
  border: 0;
  border-radius: var(--radius);
  background: none;
  color: var(--text);
  font: inherit;
  text-align: left;
  text-decoration: none;
  cursor: pointer;
}
.row:hover,
.row:focus-visible {
  background: var(--select);
  outline: none;
}
.row__label {
  flex: 1;
}
.row__check {
  color: var(--accent);
}
.prefs {
  display: flex;
  justify-content: space-between;
  gap: 8px;
  padding: 4px 8px;
}
hr {
  width: 100%;
  margin: 4px 0;
  border: 0;
  border-top: 1px solid var(--border);
}
</style>
