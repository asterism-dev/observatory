import { createRouter, createWebHistory } from 'vue-router'

import { ApiError } from '@/api'
import { useMe } from '@/composables/useMe'
import AdminAuditView from '@/views/AdminAuditView.vue'
import AdminOrgsView from '@/views/admin/AdminOrgsView.vue'
import AdminOrgView from '@/views/admin/AdminOrgView.vue'
import AdminUsersView from '@/views/admin/AdminUsersView.vue'
import LoginView from '@/views/LoginView.vue'
import InviteView from '@/views/InviteView.vue'
import OrgAuditView from '@/views/org/OrgAuditView.vue'
import LimitsView from '@/views/org/LimitsView.vue'
import RoleEditorView from '@/views/org/RoleEditorView.vue'
import RolesView from '@/views/org/RolesView.vue'
import TeamDetailView from '@/views/org/TeamDetailView.vue'
import TeamsView from '@/views/org/TeamsView.vue'
import MembersView from '@/views/org/MembersView.vue'
import OrgSettingsView from '@/views/org/OrgSettingsView.vue'
import PlaceholderView from '@/views/PlaceholderView.vue'
import ProfileView from '@/views/ProfileView.vue'

declare module 'vue-router' {
  interface RouteMeta {
    title?: string
  }
}

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', component: ProfileView },
    { path: '/login', component: LoginView, meta: { public: true } },
    { path: '/profile', component: ProfileView },
    { path: '/orgs/:orgId/dashboard', component: PlaceholderView, meta: { title: 'nav.dashboard' } },
    { path: '/orgs/:orgId/nodes', component: PlaceholderView, meta: { title: 'nav.nodes' } },
    { path: '/orgs/:orgId/vms', component: PlaceholderView, meta: { title: 'nav.vms' } },
    { path: '/orgs/:orgId/members', component: MembersView },
    { path: '/orgs/:orgId/settings', component: OrgSettingsView },
    { path: '/orgs/:orgId/teams', component: TeamsView },
    { path: '/orgs/:orgId/teams/:teamId', component: TeamDetailView },
    { path: '/orgs/:orgId/roles', component: RolesView },
    { path: '/orgs/:orgId/roles/:roleId', component: RoleEditorView },
    { path: '/orgs/:orgId/limits', component: LimitsView },
    { path: '/orgs/:orgId/audit', component: OrgAuditView },
    { path: '/invite/:token', component: InviteView },
    { path: '/admin/orgs', component: AdminOrgsView, meta: { platformAdmin: true } },
    { path: '/admin/orgs/:orgId', component: AdminOrgView, meta: { platformAdmin: true } },
    { path: '/admin/users', component: AdminUsersView, meta: { platformAdmin: true } },
    { path: '/admin/audit', component: AdminAuditView, meta: { platformAdmin: true } },
  ],
})

router.beforeEach(async (to) => {
  if (to.meta.public) return true
  const { me, activeOrg, load } = useMe()
  if (!me.value) {
    try {
      await load()
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        return { path: '/login', query: { redirect: to.fullPath } }
      }
      throw e
    }
  }
  if (to.path === '/') return activeOrg.value ? `/orgs/${activeOrg.value.id}/dashboard` : '/profile'
  if (to.meta.platformAdmin && !me.value?.user.platformAdmin) return '/profile'
  return true
})
