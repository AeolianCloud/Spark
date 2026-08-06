import { request, type LocatedResponse, type ListResponse } from './client'
import type { Image, ImageRequest } from './types'

/** 登记 cloud 镜像 */
export function createImage(body: ImageRequest): Promise<LocatedResponse<Image>> {
  return request<Image>('/images', { method: 'POST', body }) as Promise<LocatedResponse<Image>>
}

/** 镜像列表（带 zone_id 时返回该区域各启用节点镜像交集，分页；total 为 X-Total-Count 头） */
export function listImages(
  params: { zone_id?: number, limit?: number, offset?: number } = {}
): Promise<ListResponse<Image[]>> {
  return request<Image[]>('/images', { query: params }) as Promise<ListResponse<Image[]>>
}
