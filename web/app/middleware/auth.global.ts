/**
 * 全局路由守卫：未登录访问业务页 → /login；已登录访问 /login → /dashboard。
 * SPA 下 middleware 在客户端执行；此处直接读 utils/auth 的持久化令牌判定登录态，
 * 避免依赖 composable 的初始化时序（SPA 首帧加载同样经过本守卫）。
 */
import { getToken } from '~/utils/auth'

export default defineNuxtRouteMiddleware((to) => {
  const loggedIn = getToken() !== null

  // 未登录访问业务页（/login 与 /healthz 之外的路径）：导向登录页
  if (to.path !== '/login' && !loggedIn) {
    return navigateTo('/login')
  }

  // 已登录访问登录页：跳回首页（避免重复登录）
  if (to.path === '/login' && loggedIn) {
    return navigateTo('/dashboard')
  }
})
