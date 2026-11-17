import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

import { useMe } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'

export interface NavTab {
  path: string
  label: string
}

const ORG_TABS = [
  { segment: 'settings', action: 'org:update', label: 'nav.general' },
  { segment: 'members', action: 'member:read', label: 'nav.members' },
  { segment: 'teams', action: 'team:read', label: 'nav.teams' },
  { segment: 'roles', action: 'role:read', label: 'nav.roles' },
  { segment: 'limits', action: 'limit:read', label: 'nav.limits' },
  { segment: 'audit', action: 'audit:read', label: 'nav.orgAudit' },
] as const

export function useSettingsNav() {
  const { t } = useI18n()
  const { me } = useMe()
  const { org, can } = useOrg()

  const orgTabs = computed<NavTab[]>(() => {
    const id = org.value?.id
    if (!id) return []
    return ORG_TABS.filter((tab) => can(tab.action)).map((tab) => ({
      path: `/orgs/${id}/${tab.segment}`,
      label: t(tab.label),
    }))
  })

  const platformTabs = computed<NavTab[]>(() =>
    me.value?.user.platformAdmin
      ? [
          { path: '/admin/orgs', label: t('nav.adminOrgs') },
          { path: '/admin/users', label: t('nav.adminUsers') },
          { path: '/admin/audit', label: t('nav.audit') },
        ]
      : [],
  )

  return { orgTabs, platformTabs }
}
