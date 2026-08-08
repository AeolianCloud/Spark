import { request, type ApiResponse } from './client'
import type { AdminLoginRequest, AdminLoginResponse } from './types'

/**
 * 管理员登录：POST /auth/admin/login。
 * 登录请求不注入令牌（client.ts 对 /auth/* 路径豁免），响应 401 也不触发全局跳转
 * （client.ts 对登录请求豁免），由登录页直接展示认证失败信息。
 */
export async function adminLogin(body: AdminLoginRequest): Promise<ApiResponse<AdminLoginResponse>> {
  return request<AdminLoginResponse>('/auth/admin/login', { method: 'POST', body })
}
