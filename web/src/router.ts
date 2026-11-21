import { createRouter, createWebHistory } from 'vue-router'

import { ApiError } from '@/api'
import { useMe } from '@/composables/useMe'
import AdminAuditView from '@/views/AdminAuditView.vue'
import DashboardView from '@/views/DashboardView.vue'
import DisksView from '@/views/DisksView.vue'
import DevicesView from '@/views/DevicesView.vue'
import NetworksView from '@/views/NetworksView.vue'
import EnvView from '@/views/EnvView.vue'
import ImageDetailView from '@/views/ImageDetailView.vue'
import ImagesView from '@/views/ImagesView.vue'
import IntegrationDetailView from '@/views/IntegrationDetailView.vue'
import IntegrationsView from '@/views/IntegrationsView.vue'
import NodeDetailView from '@/views/NodeDetailView.vue'
import NodesView from '@/views/NodesView.vue'
import AdminOrgsView from '@/views/admin/AdminOrgsView.vue'
import AdminOrgView from '@/views/admin/AdminOrgView.vue'
import PlatformSettingsLayout from '@/views/admin/PlatformSettingsLayout.vue'
import AdminRegistryView from '@/views/admin/AdminRegistryView.vue'
import AdminUsersView from '@/views/admin/AdminUsersView.vue'
import LoginView from '@/views/LoginView.vue'
import InviteView from '@/views/InviteView.vue'
import BuilderVMsView from '@/views/org/BuilderVMsView.vue'
import OrgAuditView from '@/views/org/OrgAuditView.vue'
import LimitsView from '@/views/org/LimitsView.vue'
import RoleEditorView from '@/views/org/RoleEditorView.vue'
import RolesView from '@/views/org/RolesView.vue'
import TeamDetailView from '@/views/org/TeamDetailView.vue'
import TelemetryTokensView from '@/views/org/TelemetryTokensView.vue'
import TeamsView from '@/views/org/TeamsView.vue'
import MembersView from '@/views/org/MembersView.vue'
import OrgSettingsLayout from '@/views/org/OrgSettingsLayout.vue'
import OrgSettingsView from '@/views/org/OrgSettingsView.vue'
import ProfileView from '@/views/ProfileView.vue'
import VMDetailView from '@/views/VMDetailView.vue'
import VMsView from '@/views/VMsView.vue'

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
    { path: '/orgs/:orgId/dashboard', component: DashboardView, meta: { title: 'nav.dashboard' } },
    { path: '/orgs/:orgId/nodes', component: NodesView, meta: { title: 'nav.nodes' } },
    { path: '/orgs/:orgId/nodes/:nodeId', component: NodeDetailView },
    { path: '/orgs/:orgId/vms', component: VMsView, meta: { title: 'nav.vms' } },
    { path: '/orgs/:orgId/vms/:vmId', component: VMDetailView },
    { path: '/orgs/:orgId/disks', component: DisksView, meta: { title: 'nav.disks' } },
    { path: '/orgs/:orgId/networks', component: NetworksView, meta: { title: 'nav.networks' } },
    { path: '/orgs/:orgId/devices', component: DevicesView, meta: { title: 'nav.devices' } },
    { path: '/orgs/:orgId/env', component: EnvView, meta: { title: 'nav.env' } },
    { path: '/orgs/:orgId/images', component: ImagesView, meta: { title: 'nav.images' } },
    { path: '/orgs/:orgId/images/:imageId', component: ImageDetailView },
    { path: '/orgs/:orgId/integrations', component: IntegrationsView, meta: { title: 'nav.integrations' } },
    { path: '/orgs/:orgId/integrations/:id', component: IntegrationDetailView },
    {
      path: '/orgs/:orgId',
      component: OrgSettingsLayout,
      children: [
        { path: 'settings', component: OrgSettingsView },
        { path: 'members', component: MembersView },
        { path: 'teams', component: TeamsView },
        { path: 'teams/:teamId', component: TeamDetailView },
        { path: 'roles', component: RolesView },
        { path: 'roles/:roleId', component: RoleEditorView },
        { path: 'limits', component: LimitsView },
        { path: 'audit', component: OrgAuditView },
        { path: 'builders', component: BuilderVMsView },
        { path: 'telemetry', component: TelemetryTokensView },
      ],
    },
    { path: '/invite/:token', component: InviteView },
    {
      path: '/admin',
      component: PlatformSettingsLayout,
      meta: { platformAdmin: true },
      children: [
        { path: 'orgs', component: AdminOrgsView },
        { path: 'orgs/:orgId', component: AdminOrgView },
        { path: 'users', component: AdminUsersView },
        { path: 'audit', component: AdminAuditView },
        { path: 'registry', component: AdminRegistryView },
      ],
    },
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
