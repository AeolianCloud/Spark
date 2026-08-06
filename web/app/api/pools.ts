import { request, type ApiResponse, type LocatedResponse, type ListResponse } from './client'
import type { CreatePoolRequest, NodeResponse, Pool, SetPoolNodesRequest } from './types'

/** 创建 IP 池（自动展开为逐地址记录） */
export function createPool(body: CreatePoolRequest): Promise<LocatedResponse<Pool>> {
  return request<Pool>('/ip-pools', { method: 'POST', body }) as Promise<LocatedResponse<Pool>>
}

/** IP 池列表（可按区域过滤，分页；total 为 X-Total-Count 头） */
export function listPools(
  params: { zone_id?: number, limit?: number, offset?: number } = {}
): Promise<ListResponse<Pool[]>> {
  return request<Pool[]>('/ip-pools', { query: params }) as Promise<ListResponse<Pool[]>>
}

/** 设置 IP 池的节点白名单（整体替换） */
export function setPoolNodes(id: number, body: SetPoolNodesRequest): Promise<ApiResponse<NodeResponse[]>> {
  return request<NodeResponse[]>(`/ip-pools/${id}/nodes`, { method: 'PUT', body })
}

/** 获取 IP 池的节点白名单 */
export function getPoolNodes(id: number): Promise<ApiResponse<NodeResponse[]>> {
  return request<NodeResponse[]>(`/ip-pools/${id}/nodes`)
}
