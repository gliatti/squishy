import { createRouter, createWebHistory } from 'vue-router'

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', component: () => import('../views/ProjectsList.vue') },
    { path: '/projects/:id', component: () => import('../views/ProjectDetail.vue') },
    { path: '/projects/:id/instances/new', component: () => import('../views/AddInstance.vue') },
    { path: '/instances/:iid', component: () => import('../views/InstanceDetail.vue') },
    { path: '/instances/:iid/edit', component: () => import('../views/EditInstance.vue') },
    { path: '/migrations/:mid', component: () => import('../views/MigrationDetail.vue') },
    { path: '/runs/:id', component: () => import('../views/RunMonitor.vue') },
  ],
})
