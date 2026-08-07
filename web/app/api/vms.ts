import { request, type ApiResponse, type LocatedResponse, type ListResponse } from './client'
import type { AcceptedResponse, CreateVMRequest, ImportVMRequest, ResizeRequest, VMListItem, VMListResponse, VMOperationsResponse, VMResponse } from './types'

/** 创建 VM：分配 IP → 落库 → 异步 PVE 创建链，201 立即返回（status 为 creating） */
export function createVM(body: CreateVMRequest): Promise<LocatedResponse<VMResponse>> {
  return request<VMResponse>('/vms', { method: 'POST', body }) as Promise<LocatedResponse<VMResponse>>
}

/**
 * VM 列表（穿透式合并各节点实时状态，含 external 虚拟机，分页）。
 * data 为 VMListResponse 包装（vms + warnings）；total 为 X-Total-Count 头
 * （合并后总条数，含 external，剔除故障/禁用节点的虚拟机）。
 */
export function listVMs(params: { limit?: number, offset?: number } = {}): Promise<ListResponse<VMListResponse>> {
  return request<VMListResponse>('/vms', { query: params }) as Promise<ListResponse<VMListResponse>>
}

/** 认领已有 VM：将 PVE 上已存在的 VM 认领为本平台托管 VM（source 变更为 claimed）。
 *  ip 为 IP 地址字符串（IPv4/IPv6），可选；不传则不分配 IP（网络由 PVE 侧配置决定）。
 *  201 返回完整 VMListItem + Location */
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

/** 销毁 VM（同步且幂等，204 无响应体）。
 *  id 支持数字本地行 id 与 external 合成标识 ext-{nodeID}-{vmid}（字符串） */
export async function destroyVM(id: string | number): Promise<void> {
  // eslint-disable-next-line @typescript-eslint/no-invalid-void-type -- 204 无响应体，void 即"无响应体"语义
  await request<void>(`/vms/${id}`, { method: 'DELETE' })
}

/** 启动 VM（异步派发，202；Location 指向 GET /vms/{id}，external 标识省略该头）。
 *  id 支持数字本地行 id 与 external 合成标识 ext-{nodeID}-{vmid}（字符串） */
export function startVM(id: string | number): Promise<LocatedResponse<AcceptedResponse>> {
  return request<AcceptedResponse>(`/vms/${id}/start`, { method: 'POST' }) as Promise<LocatedResponse<AcceptedResponse>>
}

/** 关闭 VM（ACPI 优雅关机，异步派发，202；Location 指向 GET /vms/{id}，external 标识省略该头）。
 *  id 支持数字本地行 id 与 external 合成标识 ext-{nodeID}-{vmid}（字符串） */
export function stopVM(id: string | number): Promise<LocatedResponse<AcceptedResponse>> {
  return request<AcceptedResponse>(`/vms/${id}/stop`, { method: 'POST' }) as Promise<LocatedResponse<AcceptedResponse>>
}

/** 重启 VM（异步派发，202；Location 指向 GET /vms/{id}，external 标识省略该头）。
 *  id 支持数字本地行 id 与 external 合成标识 ext-{nodeID}-{vmid}（字符串） */
export function restartVM(id: string | number): Promise<LocatedResponse<AcceptedResponse>> {
  return request<AcceptedResponse>(`/vms/${id}/restart`, { method: 'POST' }) as Promise<LocatedResponse<AcceptedResponse>>
}

/**
 * VM 操作记录（审计轨迹，按时间倒序分页）。
 * id 支持数字本地行 id 与 external 合成标识 ext-{nodeID}-{vmid}（字符串）；
 * total 为 X-Total-Count 头（匹配总数）。
 */
export function listVMOperations(id: string | number, params: { limit?: number, offset?: number } = {}): Promise<ListResponse<VMOperationsResponse>> {
  return request<VMOperationsResponse>(`/vms/${id}/operations`, { query: params }) as Promise<ListResponse<VMOperationsResponse>>
}
