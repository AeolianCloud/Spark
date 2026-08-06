import { request, type ApiResponse, type LocatedResponse } from './client'
import type { NodeRequest, NodeResponse } from './types'

/** 登记 PVE 节点（api_token 只写：响应不含令牌，仅 api_token_set） */
export function createNode(zoneId: number, body: NodeRequest): Promise<LocatedResponse<NodeResponse>> {
  return request<NodeResponse>(`/zones/${zoneId}/nodes`, { method: 'POST', body }) as Promise<LocatedResponse<NodeResponse>>
}

/** 区域节点列表 */
export function listNodesByZone(zoneId: number): Promise<ApiResponse<NodeResponse[]>> {
  return request<NodeResponse[]>(`/zones/${zoneId}/nodes`)
}

/** 更新节点（空 api_token 保留原密钥；enabled 缺省时保留现值） */
export function updateNode(id: number, body: NodeRequest): Promise<ApiResponse<NodeResponse>> {
  return request<NodeResponse>(`/nodes/${id}`, { method: 'PUT', body })
}
