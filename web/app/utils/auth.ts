/**
 * 登录态持久化：令牌与管理员身份的 localStorage 存取（key 前缀 spark.）。
 * 独立为 utils 而非 composable：client.ts 请求注入直接依赖本模块，
 * 避免 client → composables 的依赖方向；路由守卫同样直接读取，不依赖 composable 初始化时序。
 * localStorage 不可用（隐私模式/禁用存储）时降级为模块级内存对象，刷新页面即失效（行为等价未登录）。
 */

/** 令牌存储 key：localStorage 与内存降级共用 */
const TOKEN_KEY = 'spark.token'
/** 身份信息存储 key（JSON 序列化） */
const IDENTITY_KEY = 'spark.admin'

/** 登录管理员身份（与契约 AdminLoginResponse 的身份字段一致） */
export interface AuthIdentity {
  /** admins 表 ID */
  admin_id: number
  /** 管理员登录名 */
  username: string
}

// localStorage 不可用时的内存降级存储
const memoryStore: { token: string | null, identity: string | null } = {
  token: null,
  identity: null
}

// localStorage 可用性探测结果缓存（null 表示未探测）。
// 隐私模式下 getItem 可能可用但 setItem 抛异常，必须以写操作探测
let storageAvailable: boolean | null = null

/** 返回可用的 localStorage；不可用（隐私模式等）返回 null 表示走内存降级 */
function safeStorage(): Storage | null {
  if (storageAvailable === null) {
    try {
      const probeKey = '__spark_storage_probe__'
      window.localStorage.setItem(probeKey, '1')
      window.localStorage.removeItem(probeKey)
      storageAvailable = true
    } catch {
      storageAvailable = false
    }
  }
  return storageAvailable ? window.localStorage : null
}

/** 读取登录令牌；未登录或存储不可用时返回 null */
export function getToken(): string | null {
  const s = safeStorage()
  return (s ? s.getItem(TOKEN_KEY) : memoryStore.token) ?? null
}

/** 持久化登录态：令牌 + 身份信息（登录成功后调用） */
export function setAuth(token: string, identity: AuthIdentity): void {
  const s = safeStorage()
  const serialized = JSON.stringify(identity)
  if (s) {
    s.setItem(TOKEN_KEY, token)
    s.setItem(IDENTITY_KEY, serialized)
  } else {
    memoryStore.token = token
    memoryStore.identity = serialized
  }
}

/** 清除登录态（登出 / 令牌失效 401 时调用） */
export function clearAuth(): void {
  const s = safeStorage()
  if (s) {
    s.removeItem(TOKEN_KEY)
    s.removeItem(IDENTITY_KEY)
  } else {
    memoryStore.token = null
    memoryStore.identity = null
  }
}

/** 读取登录管理员身份；存储无身份或内容被篡改（解析失败）时返回 null */
export function getIdentity(): AuthIdentity | null {
  const s = safeStorage()
  const raw = (s ? s.getItem(IDENTITY_KEY) : memoryStore.identity) ?? null
  if (raw === null) return null
  try {
    const parsed: unknown = JSON.parse(raw)
    // 防御非预期结构：仅接受含 username 字符串与 admin_id 数字的对象
    if (typeof parsed === 'object' && parsed !== null
      && typeof (parsed as { username?: unknown }).username === 'string'
      && typeof (parsed as { admin_id?: unknown }).admin_id === 'number') {
      return parsed as AuthIdentity
    }
  } catch {
    // 存储被篡改/格式异常：视为未登录（返回 null），待下次 setAuth 覆盖
  }
  return null
}
