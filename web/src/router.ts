import { createRouter, createWebHistory } from 'vue-router'

import { ApiError } from '@/api'
import { useMe } from '@/composables/useMe'
import AdminAuditView from '@/views/AdminAuditView.vue'
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
import ProfileView from '@/views/ProfileView.vue'

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', component: ProfileView },
    { path: '/login', component: LoginView, meta: { public: true } },
    { path: '/profile', component: ProfileView },
    { path: '/orgs/:orgId/members', component: MembersView },
    { path: '/orgs/:orgId/settings', component: OrgSettingsView },
    { path: '/orgs/:orgId/teams', component: TeamsView },
    { path: '/orgs/:orgId/teams/:teamId', component: TeamDetailView },
    { path: '/orgs/:orgId/roles', component: RolesView },
    { path: '/orgs/:orgId/roles/:roleId', component: RoleEditorView },
    { path: '/orgs/:orgId/limits', component: LimitsView },
    { path: '/orgs/:orgId/audit', component: OrgAuditView },
    { path: '/invite/:token', component: InviteView },
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
  if (to.path === '/') return activeOrg.value ? `/orgs/${activeOrg.value.id}/members` : '/profile'
  if (to.meta.platformAdmin && !me.value?.user.platformAdmin) return '/profile'
  return true
})
