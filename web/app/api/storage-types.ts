import { request, type ApiResponse, type ListResponse } from './client'
import type { StorageScanSummary, StorageType, StorageTypeUpdateRequest } from './types'

/**
 * 存储类型列表（分页，可选按区域过滤；total 为 X-Total-Count 头）。
 * 存储类型由扫描（scanStorageTypes）从 PVE 自动同步产生，不再支持手动登记。
 */
export function listStorageTypes(params: { limit?: number, offset?: number, zone_id?: number } = {}): Promise<ListResponse<StorageType[]>> {
  return request<StorageType[]>('/storage-types', { query: params }) as Promise<ListResponse<StorageType[]>>
}

/** 手动触发指定区域的存储扫描：从该区域集群读取存储清单并同步本地，返回同步摘要 */
export function scanStorageTypes(zoneId: number): Promise<ApiResponse<StorageScanSummary>> {
  return request<StorageScanSummary>('/storage-types/scan', { method: 'POST', query: { zone_id: zoneId } })
}

/** 存储类型详情 */
export function getStorageType(id: number): Promise<ApiResponse<StorageType>> {
  return request<StorageType>(`/storage-types/${id}`)
}

/**
 * 更新存储类型管理员元数据：name 为业务名（可空，传空串表示置空为 NULL，
 * 展示回退到 pve_storage）；enabled 为启用开关。pve_storage 是扫描权威字段，不可修改。
 */
export function updateStorageType(id: number, body: StorageTypeUpdateRequest): Promise<ApiResponse<StorageType>> {
  return request<StorageType>(`/storage-types/${id}`, { method: 'PUT', body })
}

/** 删除存储类型（204 无响应体） */
export async function deleteStorageType(id: number): Promise<void> {
  // eslint-disable-next-line @typescript-eslint/no-invalid-void-type -- 204 无响应体，void 即"无响应体"语义
  await request<void>(`/storage-types/${id}`, { method: 'DELETE' })
}
