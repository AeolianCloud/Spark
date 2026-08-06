import { request, type LocatedResponse, type ListResponse } from './client'
import type { ZoneCreateRequest, ZoneResponse } from './types'

/** 创建区域 */
export function createZone(body: ZoneCreateRequest): Promise<LocatedResponse<ZoneResponse>> {
  return request<ZoneResponse>('/zones', { method: 'POST', body }) as Promise<LocatedResponse<ZoneResponse>>
}

/** 区域列表（含各区域完整节点列表，分页；total 为 X-Total-Count 头） */
export function listZones(params: { limit?: number, offset?: number } = {}): Promise<ListResponse<ZoneResponse[]>> {
  return request<ZoneResponse[]>('/zones', { query: params }) as Promise<ListResponse<ZoneResponse[]>>
}
