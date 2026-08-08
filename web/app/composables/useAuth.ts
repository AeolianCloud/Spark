/**
 * 登录状态 composable：响应式 isLoggedIn/identity 与 login/logout 动作。
 * 状态读写底层走 utils/auth（localStorage 持久化），此处只做响应式包装；
 * 模块级状态在 SPA 单实例下跨页面共享（布局页脚与业务页读取同一份状态）。
 */
import { adminLogin } from '~/api/auth'
import { getToken, setAuth, clearAuth, getIdentity, type AuthIdentity } from '~/utils/auth'

// 模块级共享状态：初始化时机为模块加载首帧（SPA 下 localStorage 同步可读，无时序问题）
const token = ref<string | null>(getToken())
const identity = ref<AuthIdentity | null>(getIdentity())

export function useAuth() {
  /** 是否已登录：以持久化令牌是否存在判定 */
  const isLoggedIn = computed(() => token.value !== null)

  /** 管理员登录：调 adminLogin 成功后持久化令牌与身份并刷新响应式状态 */
  async function login(username: string, password: string): Promise<void> {
    const res = await adminLogin({ username, password })
    setAuth(res.data.token, { admin_id: res.data.admin_id, username: res.data.username })
    token.value = res.data.token
    identity.value = { admin_id: res.data.admin_id, username: res.data.username }
  }

  /** 登出：清除本地登录态并全量跳转登录页（保证 SPA 状态干净） */
  function logout(): void {
    clearAuth()
    token.value = null
    identity.value = null
    window.location.assign('/login')
  }

  return { isLoggedIn, identity, login, logout }
}
