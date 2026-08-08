import type { components } from './generated/schema'
import { getToken, clearAuth } from '../utils/auth'

/** 契约错误码：与 docs/openapi.yaml ErrorDetail.code 枚举一致（由生成类型强约束） */
export type ErrorCode = components['schemas']['ErrorDetail']['code']

/**
 * API 错误：携带 HTTP status、契约错误码 code 与脱敏后的 message，
 * 供页面统一展示（错误码唯一可依赖，message 仅供人阅读）。
 * code 为 'unknown' 表示响应体不符合契约错误结构（防御性兜底）。
 */
export class ApiError extends Error {
  readonly status: number
  readonly code: ErrorCode | 'unknown'

  constructor(status: number, code: ErrorCode | 'unknown', message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

/** 统一响应包装：data 为解析后的响应体，total/location 为响应头 */
export interface ApiResponse<T> {
  data: T
  /** X-Total-Count 响应头：列表端点（分页前总条数） */
  total?: number
  /** Location 响应头：201 创建 / 202 异步受理端点 */
  location?: string
}

/** 列表端点返回：契约保证 200 恒携带 X-Total-Count，故 total 必存在（data 为端点实际响应体） */
export type ListResponse<T> = ApiResponse<T> & { total: number }

/** 带 Location 头的响应（201 创建成功 / 202 异步受理） */
export type LocatedResponse<T> = ApiResponse<T> & { location: string }

/** API 基础路径：dev 由 nuxt devProxy 转发，生产由 nginx 反代，均为同源 /api */
let apiBase = '/api'

/** 覆盖 API 基础路径（去掉末尾斜杠），测试或特殊部署时可调整 */
export function setApiBaseUrl(base: string): void {
  apiBase = base.replace(/\/+$/, '')
}

interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'
  /** 查询参数：undefined/null 值自动跳过 */
  query?: Record<string, string | number | undefined | null>
  /** 请求体：自动 JSON 序列化并设置 Content-Type */
  body?: unknown
  /** 额外视为成功响应的状态码（如 healthz 的 503 degraded） */
  okStatuses?: number[]
  signal?: AbortSignal
}

/** 类型守卫：响应体是否符合契约错误结构 {"error":{"code","message"}} */
function isErrorBody(value: unknown): value is { error: { code: string, message?: string } } {
  if (typeof value !== 'object' || value === null) return false
  const error = (value as { error?: unknown }).error
  if (typeof error !== 'object' || error === null) return false
  return typeof (error as { code?: unknown }).code === 'string'
}

/** 解析失败响应为契约错误结构并抛出自定义 ApiError；非 JSON 响应体走兜底 */
async function parseError(res: Response): Promise<ApiError> {
  let code: ErrorCode | 'unknown' = 'unknown'
  let message = `HTTP ${res.status} ${res.statusText}`
  try {
    const body: unknown = await res.json()
    if (isErrorBody(body)) {
      code = body.error.code as ErrorCode | 'unknown'
      if (body.error.message) message = body.error.message
    }
  } catch {
    // 非 JSON 响应体：保留兜底 message
  }
  return new ApiError(res.status, code, message)
}

/** 统一请求入口：拼接 baseURL、序列化查询参数与请求体、注入 Bearer 令牌、解析响应头与 JSON 响应体 */
async function request<T>(path: string, options: RequestOptions = {}): Promise<ApiResponse<T>> {
  const { method = 'GET', query, body, okStatuses, signal } = options

  let url = `${apiBase}${path}`
  if (query) {
    const params = new URLSearchParams()
    for (const [key, value] of Object.entries(query)) {
      if (value !== undefined && value !== null) params.set(key, String(value))
    }
    const qs = params.toString()
    if (qs) url += `?${qs}`
  }

  const headers: Record<string, string> = {}
  // 登录路径与健康检查不注入令牌（后端安全规则：/auth/*、/healthz 匿名可达）
  const authExempt = path.startsWith('/auth/') || path === '/healthz'
  if (!authExempt) {
    const token = getToken()
    if (token) headers['Authorization'] = `Bearer ${token}`
  }
  let payload: string | undefined
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json'
    payload = JSON.stringify(body)
  }

  let res: Response
  try {
    res = await fetch(url, { method, headers, body: payload, signal })
  } catch (err) {
    // 网络层失败（断网、代理不可达、CORS 等）：fetch 仅对这类错误 reject 原生 TypeError，
    // 统一包装为 ApiError（status 0 + code 'unknown'），保证所有失败路径均可被统一错误组件消费
    const detail = err instanceof Error ? `（原始错误：${err.message}）` : ''
    throw new ApiError(0, 'unknown', `网络请求失败，请检查网络连接${detail}`)
  }

  // 契约外的成功状态码（如 healthz 503 degraded）按数据响应处理
  if (!res.ok && !okStatuses?.includes(res.status)) {
    const err = await parseError(res)
    // 令牌缺失/过期/被拒：清除本地登录态并全量跳转登录页（SPA 下保证状态干净）；
    // 登录请求自身的 401 豁免（由登录页直接展示认证失败，避免跳转死循环）
    if (res.status === 401 && !authExempt) {
      clearAuth()
      window.location.assign('/login')
    }
    throw err
  }

  const result: ApiResponse<T> = { data: undefined as T }

  const totalRaw = res.headers.get('X-Total-Count')
  if (totalRaw !== null) {
    // 防御非法值（空字符串等会得到 0/NaN）：不合法时忽略该头，保持 total 未定义
    const n = Number(totalRaw)
    if (Number.isFinite(n)) result.total = n
  }

  const location = res.headers.get('Location')
  if (location) {
    // Location 为相对 API 路径（如 /vms/1），页面消费时需拼接 apiBase 再使用
    result.location = location
  }

  // 204 无响应体
  if (res.status === 204) return result

  try {
    result.data = (await res.json()) as T
  } catch (err) {
    // 契约要求成功响应均为 JSON；此处防御非 JSON 成功响应
    // （如反代故障时返回的 HTML 错误页），保留 data 未定义并告警便于排查
    console.warn(`响应体解析失败：HTTP ${res.status} 响应体不是预期 JSON，请检查反向代理/网关配置（url=${url}${err instanceof Error ? `，原始错误：${err.message}` : ''}）`)
  }
  return result
}

export { request }
