import { request, type ApiResponse, type LocatedResponse, type ListResponse } from './client'
import type { AcceptedResponse, CreateVMRequest, ImportVMRequest, ResizeRequest, UnmanagedVMListResponse, VMListItem, VMListResponse, VMResponse } from './types'

/** 创建 VM：分配 IP → 落库 → 异步 PVE 创建链，201 立即返回（status 为 creating） */
export function createVM(body: CreateVMRequest): Promise<LocatedResponse<VMResponse>> {
  return request<VMResponse>('/vms', { method: 'POST', body }) as Promise<LocatedResponse<VMResponse>>
}

/**
 * VM 列表（穿透式合并各节点实时状态，分页）。
 * data 为 VMListResponse 包装（vms + warnings）；total 为 X-Total-Count 头（本地 VM 总行数）。
 */
export function listVMs(params: { limit?: number, offset?: number } = {}): Promise<ListResponse<VMListResponse>> {
  return request<VMListResponse>('/vms', { query: params }) as Promise<ListResponse<VMListResponse>>
}

/** 未纳管 VM 列表：节点上 PVE 已存在、本平台尚未登记（供导入弹窗选择候选） */
export function listUnmanagedVMs(params: { node_id: number }): Promise<ApiResponse<UnmanagedVMListResponse>> {
  return request<UnmanagedVMListResponse>('/vms/unmanaged', { query: params })
}

/** 导入已有 VM：将 PVE 上已存在的 VM 纳入本平台纳管，201 返回完整 VMListItem + Location */
export function importVM(body: ImportVMRequest): Promise<LocatedResponse<VMListItem>> {
  return request<VMListItem>('/vms/import', { method: 'POST', body }) as Promise<LocatedResponse<VMListItem>>
}

/** VM 详情（PVE 实时穿透状态） */
export function getVM(id: number): Promise<ApiResponse<VMListItem>> {
  return request<VMListItem>(`/vms/${id}`)
}

/** 升降配（JSON Merge Patch 部分更新语义：仅应用请求体出现的字段，磁盘只增不减） */
export function resizeVM(id: number, body: ResizeRequest): Promise<ApiResponse<VMListItem>> {
  return request<VMListItem>(`/vms/${id}`, { method: 'PATCH', body })
}

/** 销毁 VM（同步且幂等，204 无响应体） */
export async function destroyVM(id: number): Promise<void> {
  // eslint-disable-next-line @typescript-eslint/no-invalid-void-type -- 204 无响应体，void 即"无响应体"语义
  await request<void>(`/vms/${id}`, { method: 'DELETE' })
}

/** 启动 VM（异步派发，202；Location 指向 GET /vms/{id}） */
export function startVM(id: number): Promise<LocatedResponse<AcceptedResponse>> {
  return request<AcceptedResponse>(`/vms/${id}/start`, { method: 'POST' }) as Promise<LocatedResponse<AcceptedResponse>>
}

/** 关闭 VM（ACPI 优雅关机，异步派发，202；Location 指向 GET /vms/{id}） */
export function stopVM(id: number): Promise<LocatedResponse<AcceptedResponse>> {
  return request<AcceptedResponse>(`/vms/${id}/stop`, { method: 'POST' }) as Promise<LocatedResponse<AcceptedResponse>>
}

/** 重启 VM（异步派发，202；Location 指向 GET /vms/{id}） */
export function restartVM(id: number): Promise<LocatedResponse<AcceptedResponse>> {
  return request<AcceptedResponse>(`/vms/${id}/restart`, { method: 'POST' }) as Promise<LocatedResponse<AcceptedResponse>>
}
