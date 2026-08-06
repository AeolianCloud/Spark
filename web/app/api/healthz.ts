import { request, ApiError, type ApiResponse } from './client'
import type { HealthResponse } from './types'

/**
 * 健康检查：200 正常；503 degraded 仍属契约定义的正常响应（响应体同为 HealthResponse）。
 * 兜底校验响应体 status：反代故障时可能返回 HTML 错误页（响应体非 HealthResponse），
 * 通过校验 status 字段与真 degraded 区分，保证调用方不会把反代错误页误判为服务降级。
 */
export async function healthz(): Promise<ApiResponse<HealthResponse>> {
  const res = await request<HealthResponse>('/healthz', { okStatuses: [503] })
  const status = res.data?.status
  if (status !== 'ok' && status !== 'degraded') {
    // 反代错误页等非契约响应：request 不透出 HTTP 状态码，统一以 0 + 'unknown' 兜底标识
    throw new ApiError(0, 'unknown', '健康检查失败：后端/网关返回了非预期内容（疑似反代错误页而非契约响应），请检查服务状态')
  }
  return res
}
