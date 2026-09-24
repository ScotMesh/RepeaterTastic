import { createRouter, createWebHistory } from 'vue-router'
import { api, setUnauthorizedHandler, token } from '@/api/client'
import { newBuild } from '@/store/live'
import AppShell from '@/components/layout/AppShell.vue'

declare module 'vue-router' {
  interface RouteMeta {
    public?: boolean
    title?: string
    subtitle?: string
  }
}

export const router = createRouter({
  history: createWebHistory(),
  scrollBehavior: () => ({ top: 0 }),
  routes: [
    { path: '/setup', name: 'setup', component: () => import('@/views/Setup.vue'), meta: { public: true, title: 'Setup' } },
    { path: '/login', name: 'login', component: () => import('@/views/Login.vue'), meta: { public: true, title: 'Sign in' } },
    {
      path: '/',
      component: AppShell,
      children: [
        { path: '', name: 'dashboard', component: () => import('@/views/Dashboard.vue'), meta: { title: 'Dashboard' } },
        { path: 'identities', name: 'identities', component: () => import('@/views/Identities.vue'), meta: { title: 'Identities' } },
        { path: 'chat/:identity?/:conversation?', name: 'chat', component: () => import('@/views/Chat.vue'), meta: { title: 'Chat' } },
        { path: 'channels', name: 'channels', component: () => import('@/views/Channels.vue'), meta: { title: 'Channels' } },
        { path: 'nodes', name: 'nodes', component: () => import('@/views/Nodes.vue'), meta: { title: 'Nodes & map' } },
        { path: 'packets', name: 'packets', component: () => import('@/views/Packets.vue'), meta: { title: 'Packets' } },
        { path: 'statistics', name: 'statistics', component: () => import('@/views/Statistics.vue'), meta: { title: 'Statistics' } },
        { path: 'links', name: 'links', component: () => import('@/views/Links.vue'), meta: { title: 'Links' } },
        { path: 'sensors', name: 'sensors', component: () => import('@/views/Sensors.vue'), meta: { title: 'Sensors' } },
        { path: 'config/:tab?', name: 'config', component: () => import('@/views/Configuration.vue'), meta: { title: 'Configuration' } },
        { path: 'logs', name: 'logs', component: () => import('@/views/Logs.vue'), meta: { title: 'Logs' } },
        {
          path: 'plugins',
          children: [
            { path: '', name: 'plugins', component: () => import('@/views/Plugins.vue'), meta: { title: 'Plugins' } },
            { path: ':id/:tab?', name: 'plugin', component: () => import('@/views/Plugin.vue'), meta: { title: 'Plugin' } },
          ],
        },
      ],
    },
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
})

let setupChecked: boolean | null = null

export async function setupNeeded(force = false): Promise<boolean> {
  if (setupChecked !== null && !force) return setupChecked
  try {
    const r = await api.get<{ needed: boolean }>('/setup')
    setupChecked = r.needed
  } catch {
    setupChecked = false
  }
  return setupChecked
}

export function markSetupDone() {
  setupChecked = false
}

router.beforeEach(async (to, from) => {
  // The daemon was updated while this tab was open: load the new GUI rather than keep running the old one.
  if (newBuild.available && from.matched.length) {
    window.location.assign(router.resolve(to).href)
    return false
  }
  const needed = await setupNeeded()
  if (needed) return to.name === 'setup' ? true : { name: 'setup' }
  if (to.name === 'setup') return { name: token.value ? 'dashboard' : 'login' }
  if (!to.meta.public && !token.value) return { name: 'login', query: to.fullPath !== '/' ? { next: to.fullPath } : {} }
  if (to.name === 'login' && token.value) return { name: 'dashboard' }
  return true
})

router.afterEach((to) => {
  document.title = to.meta.title ? `${to.meta.title} · RepeaterTastic` : 'RepeaterTastic'
})

setUnauthorizedHandler(() => {
  const cur = router.currentRoute.value
  if (!cur.meta.public) router.replace({ name: 'login', query: { next: cur.fullPath } })
})
