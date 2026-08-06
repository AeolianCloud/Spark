/**
 * 契约类型统一出口：页面从 `~/api` 导入类型即可，无需直接引用生成目录。
 * 生成物 schema.d.ts 为契约镜像，禁止手改；此处仅做类型别名重导出。
 */
import type { components } from './generated/schema'

export type HealthResponse = components['schemas']['HealthResponse']
export type ZoneCreateRequest = components['schemas']['ZoneCreateRequest']
export type ZoneResponse = components['schemas']['ZoneResponse']
export type NodeRequest = components['schemas']['NodeRequest']
export type NodeResponse = components['schemas']['NodeResponse']
export type CreatePoolRequest = components['schemas']['CreatePoolRequest']
export type Pool = components['schemas']['Pool']
export type SetPoolNodesRequest = components['schemas']['SetPoolNodesRequest']
export type StorageTypeRequest = components['schemas']['StorageTypeRequest']
export type StorageType = components['schemas']['StorageType']
export type ImageRequest = components['schemas']['ImageRequest']
export type Image = components['schemas']['Image']
export type CreateVMRequest = components['schemas']['CreateVMRequest']
export type VMResponse = components['schemas']['VMResponse']
export type VMListItem = components['schemas']['VMListItem']
export type NodeWarning = components['schemas']['NodeWarning']
export type VMListResponse = components['schemas']['VMListResponse']
export type ResizeRequest = components['schemas']['ResizeRequest']
export type AcceptedResponse = components['schemas']['AcceptedResponse']
export type ErrorDetail = components['schemas']['ErrorDetail']
export type ErrorBody = components['schemas']['ErrorBody']
