import { request, type ApiResponse, type LocatedResponse, type ListResponse } from './client'
import type { StorageType, StorageTypeRequest } from './types'

/** 登记存储类型 */
export function createStorageType(body: StorageTypeRequest): Promise<LocatedResponse<StorageType>> {
  return request<StorageType>('/storage-types', { method: 'POST', body }) as Promise<LocatedResponse<StorageType>>
}

/** 存储类型列表（分页；total 为 X-Total-Count 头） */
export function listStorageTypes(params: { limit?: number, offset?: number } = {}): Promise<ListResponse<StorageType[]>> {
  return request<StorageType[]>('/storage-types', { query: params }) as Promise<ListResponse<StorageType[]>>
}

/** 存储类型详情 */
export function getStorageType(id: number): Promise<ApiResponse<StorageType>> {
  return request<StorageType>(`/storage-types/${id}`)
}

/** 更新存储类型 */
export function updateStorageType(id: number, body: StorageTypeRequest): Promise<ApiResponse<StorageType>> {
  return request<StorageType>(`/storage-types/${id}`, { method: 'PUT', body })
}

/** 删除存储类型（204 无响应体） */
export async function deleteStorageType(id: number): Promise<void> {
  // eslint-disable-next-line @typescript-eslint/no-invalid-void-type -- 204 无响应体，void 即"无响应体"语义
  await request<void>(`/storage-types/${id}`, { method: 'DELETE' })
}
