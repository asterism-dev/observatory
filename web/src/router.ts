import { createRouter, createWebHistory } from 'vue-router'

import { ApiError } from '@/api'
import { useMe } from '@/composables/useMe'
import AdminAuditView from '@/views/AdminAuditView.vue'
import LoginView from '@/views/LoginView.vue'
import ProfileView from '@/views/ProfileView.vue'

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', redirect: '/profile' },
    { path: '/login', component: LoginView, meta: { public: true } },
    { path: '/profile', component: ProfileView },
    { path: '/admin/audit', component: AdminAuditView, meta: { platformAdmin: true } },
  ],
})

router.beforeEach(async (to) => {
  if (to.meta.public) return true
  const { me, load } = useMe()
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
  if (to.meta.platformAdmin && !me.value?.user.platformAdmin) return '/profile'
  return true
})
