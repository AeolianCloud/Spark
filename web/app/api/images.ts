import { request, type ApiResponse, type LocatedResponse, type ListResponse } from './client'
import type {
  Image,
  ImageDownloadRequest,
  ImageOperation,
  ImageRequest,
  ImageZoneItem,
  NodeImageStatus
} from './types'

/** 登记 cloud 镜像（201 + Location 指向 GET /images/{id}） */
export function createImage(body: ImageRequest): Promise<LocatedResponse<Image>> {
  return request<Image>('/images', { method: 'POST', body }) as Promise<LocatedResponse<Image>>
}

/** 镜像列表（不带 zone_id：返回全部已登记镜像，分页；total 为 X-Total-Count 头） */
export function listImages(
  params: { limit?: number, offset?: number } = {}
): Promise<ListResponse<Image[]>> {
  return request<Image[]>('/images', { query: params }) as Promise<ListResponse<Image[]>>
}

/**
 * 区域过滤镜像列表（带 zone_id）：返回该区域各启用节点上的镜像存在状态
 * （ImageZoneItem[]：image + 内嵌 nodes，分页；total 为 X-Total-Count 头）。
 * 与无区域分支的响应结构不同，故拆分为独立函数以保持返回类型清晰。
 */
export function listImagesByZone(
  zoneId: number,
  params: { limit?: number, offset?: number } = {}
): Promise<ListResponse<ImageZoneItem[]>> {
  return request<ImageZoneItem[]>('/images', { query: { zone_id: zoneId, ...params } }) as Promise<ListResponse<ImageZoneItem[]>>
}

/** 镜像详情 */
export function getImage(id: number): Promise<ApiResponse<Image>> {
  return request<Image>(`/images/${id}`)
}

/** 镜像在各启用节点上的存在状态（不带 zone_id 时扫描全部启用节点；单节点扫描失败降级为未下载） */
export function getImageNodeStatus(id: number, params: { zone_id?: number } = {}): Promise<ApiResponse<NodeImageStatus[]>> {
  return request<NodeImageStatus[]>(`/images/${id}/nodes-status`, { query: params })
}

/**
 * 受理镜像下载（202 异步受理；node_ids 与 zone_id 二选一指定目标，均不提供返回 400；
 * 同一镜像同一节点已有 running 下载时 409）。data 为本次创建的每节点一条 running 记录；
 * Location 头指向 GET /images/{id}/operations，供前端轮询下载进度。
 */
export function downloadImage(id: number, body: ImageDownloadRequest): Promise<LocatedResponse<ImageOperation[]>> {
  return request<ImageOperation[]>(`/images/${id}/download`, { method: 'POST', body }) as Promise<LocatedResponse<ImageOperation[]>>
}

/** 镜像下载操作历史（按时间倒序分页；total 为 X-Total-Count 头） */
export function listImageOperations(
  id: number,
  params: { limit?: number, offset?: number } = {}
): Promise<ListResponse<ImageOperation[]>> {
  return request<ImageOperation[]>(`/images/${id}/operations`, { query: params }) as Promise<ListResponse<ImageOperation[]>>
}
