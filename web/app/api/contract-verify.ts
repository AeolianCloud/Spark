/**
 * API client 契约一致性校验（type-level，编译期生效，无运行时产物）。
 *
 * 校验目标：app/api 下各端点封装函数的请求体/响应体类型与
 * docs/openapi.yaml（经生成物 schema.d.ts）字段一一对应；
 * 契约新增/删除/改名端点或字段时，此处断言在 `npm run typecheck` 中失败。
 */
import type { components, operations } from './generated/schema'
import type { createVM, destroyVM, getVM, importVM, listVMOperations, listVMs, resizeVM, restartVM, startVM, stopVM } from './vms'
import type { createNode, updateNode } from './nodes'
import type { createPool, listPools, setPoolNodes } from './pools'
import type { createImage, downloadImage, getImage, getImageNodeStatus, listImageOperations, listImages, listImagesByZone } from './images'
import type { createStorageType, listStorageTypes, updateStorageType } from './storage-types'
import type { createZone, listZones } from './zones'
import type { ApiResponse, LocatedResponse, ListResponse } from './client'
import type { AcceptedResponse, Image, ImageDownloadRequest, ImageOperation, ImageZoneItem, NodeImageStatus, NodeResponse, StorageType, VMListItem, VMListResponse, VMOperation, VMOperationsResponse, VMResponse, ZoneResponse } from './types'

/** 恒真断言：约束类型推导结果必须为 true */
type Assert<T extends true> = T
/** 结构完全相等（含可空性） */
type Equal<A, B> = (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2) ? true : false
/** 取对象必填键 */
type RequiredKeys<T> = { [K in keyof T]-?: undefined extends T[K] ? never : K }[keyof T]
/** 取对象可选键 */
type OptionalKeys<T> = Exclude<keyof T, RequiredKeys<T>>

/* ---------------------------------- 2.2：31 端点全覆盖 ---------------------------------- */

type OpKeys = keyof operations
type _Assert31Ops = Assert<
  Equal<
    OpKeys,
    | 'healthz' | 'createZone' | 'listZones' | 'createNode' | 'listNodesByZone' | 'updateNode'
    | 'createPool' | 'listPools' | 'setPoolNodes' | 'getPoolNodes'
    | 'createStorageType' | 'listStorageTypes' | 'getStorageType' | 'updateStorageType' | 'deleteStorageType'
    | 'createImage' | 'listImages' | 'getImage' | 'listImageNodeStatus' | 'downloadImage' | 'listImageOperations'
    | 'createVM' | 'listVMs' | 'getVM' | 'resizeVM' | 'destroyVM' | 'startVM' | 'stopVM' | 'restartVM'
    | 'listVMOperations' | 'importVM'
  >
>

/* ---------------------------------- 2.4：写操作请求体/响应体校验 ---------------------------------- */

// createVM：CreateVMRequest 全字段必填，与契约一一对应
type _AssertCreateVMReq = Assert<Equal<keyof components['schemas']['CreateVMRequest'],
  'name' | 'cpu' | 'mem_mb' | 'disk_gb' | 'image_id' | 'storage_type_id' | 'zone_id' | 'password'>>
type _AssertCreateVMReqRequired = Assert<Equal<RequiredKeys<components['schemas']['CreateVMRequest']>,
  'name' | 'cpu' | 'mem_mb' | 'disk_gb' | 'image_id' | 'storage_type_id' | 'zone_id' | 'password'>>
// createVM 函数签名：请求体即 CreateVMRequest；响应 201 为 VMResponse + Location
type _AssertCreateVMFn = Assert<Equal<Parameters<typeof createVM>[0], components['schemas']['CreateVMRequest']>>
type _AssertCreateVMRes = Assert<Equal<ReturnType<typeof createVM>, Promise<LocatedResponse<VMResponse>>>>

// resizeVM：ResizeRequest 部分更新语义（cpu/mem_mb/disk_gb 均可选），minProperties 由运行时校验
type _AssertResizeKeys = Assert<Equal<keyof components['schemas']['ResizeRequest'], 'cpu' | 'mem_mb' | 'disk_gb'>>
type _AssertResizeOptional = Assert<Equal<OptionalKeys<components['schemas']['ResizeRequest']>,
  'cpu' | 'mem_mb' | 'disk_gb'>>
type _AssertResizeFn = Assert<Equal<Parameters<typeof resizeVM>[1], components['schemas']['ResizeRequest']>>
type _AssertResizeRes = Assert<Equal<ReturnType<typeof resizeVM>, Promise<ApiResponse<VMListItem>>>>

// destroyVM：204 无响应体；封装函数返回 void
type _AssertDestroy204NoBody = Assert<operations['destroyVM']['responses'][204] extends { content?: never } ? true : false>
type _AssertDestroyFn = Assert<Equal<ReturnType<typeof destroyVM>, Promise<void>>>

// createNode/updateNode：NodeRequest 请求体（api_token 只写），响应 NodeResponse 无 api_token 字段
type _AssertNodeReq = Assert<Equal<keyof components['schemas']['NodeRequest'],
  'name' | 'pve_name' | 'host' | 'api_user' | 'api_token' | 'enabled'>>
type _AssertNodeReqRequired = Assert<Equal<RequiredKeys<components['schemas']['NodeRequest']>,
  'name' | 'host' | 'api_user' | 'api_token'>>
type _AssertNodeResNoToken = Assert<'api_token' extends keyof components['schemas']['NodeResponse'] ? false : true>
type _AssertNodeResTokenSet = Assert<Equal<components['schemas']['NodeResponse']['api_token_set'], boolean>>
type _AssertCreateNodeFn = Assert<Equal<Parameters<typeof createNode>[1], components['schemas']['NodeRequest']>>
type _AssertUpdateNodeFn = Assert<Equal<Parameters<typeof updateNode>[1], components['schemas']['NodeRequest']>>
type _AssertNodeRes = Assert<Equal<ReturnType<typeof updateNode>, Promise<ApiResponse<NodeResponse>>>>

// createPool / setPoolNodes：请求体字段一一对应
type _AssertPoolReq = Assert<Equal<keyof components['schemas']['CreatePoolRequest'],
  'zone_id' | 'name' | 'network_cidr' | 'gateway' | 'dns'>>
type _AssertPoolReqRequired = Assert<Equal<RequiredKeys<components['schemas']['CreatePoolRequest']>,
  'zone_id' | 'name' | 'network_cidr' | 'gateway' | 'dns'>>
type _AssertCreatePoolFn = Assert<Equal<Parameters<typeof createPool>[0], components['schemas']['CreatePoolRequest']>>
type _AssertSetPoolNodes = Assert<Equal<components['schemas']['SetPoolNodesRequest'],
  { node_ids: number[] }>>
type _AssertSetPoolNodesFn = Assert<Equal<Parameters<typeof setPoolNodes>[1], components['schemas']['SetPoolNodesRequest']>>

// listVMs：响应体为 VMListResponse 包装（vms + warnings），total 来自 X-Total-Count 头
type _AssertVMListBody = Assert<Equal<components['schemas']['VMListResponse']['vms'],
  components['schemas']['VMListItem'][]>>
type _AssertVMListWarnings = Assert<Equal<components['schemas']['VMListResponse']['warnings'],
  components['schemas']['NodeWarning'][]>>
type _AssertListVMsFn = Assert<Equal<ReturnType<typeof listVMs>, Promise<ListResponse<VMListResponse>>>>
type _AssertListVMsTotalHeader = Assert<
  'X-Total-Count' extends keyof operations['listVMs']['responses'][200]['headers'] ? true : false
>
type _AssertCreateVMLocationHeader = Assert<
  'Location' extends keyof operations['createVM']['responses'][201]['headers'] ? true : false
>

// createZone：ZoneCreateRequest 仅 name 必填；响应 201 为 ZoneResponse + Location
type _AssertZoneReq = Assert<Equal<keyof components['schemas']['ZoneCreateRequest'], 'name'>>
type _AssertZoneReqRequired = Assert<Equal<RequiredKeys<components['schemas']['ZoneCreateRequest']>, 'name'>>
type _AssertCreateZoneFn = Assert<Equal<Parameters<typeof createZone>[0], components['schemas']['ZoneCreateRequest']>>
type _AssertCreateZoneRes = Assert<Equal<ReturnType<typeof createZone>, Promise<LocatedResponse<ZoneResponse>>>>
type _AssertZoneContent = Assert<Equal<
  operations['createZone']['responses'][201]['content']['application/json'],
  components['schemas']['ZoneResponse']
>>

// createImage：ImageRequest 全字段必填（name/default_user/download_url，无 node_images）；
// 响应 201 为 Image + Location
type _AssertImageReq = Assert<Equal<keyof components['schemas']['ImageRequest'],
  'name' | 'default_user' | 'download_url'>>
type _AssertImageReqRequired = Assert<Equal<RequiredKeys<components['schemas']['ImageRequest']>,
  'name' | 'default_user' | 'download_url'>>
type _AssertImageReqNoNodeImages = Assert<'node_images' extends keyof components['schemas']['ImageRequest'] ? false : true>
type _AssertCreateImageFn = Assert<Equal<Parameters<typeof createImage>[0], components['schemas']['ImageRequest']>>
type _AssertCreateImageRes = Assert<Equal<ReturnType<typeof createImage>, Promise<LocatedResponse<Image>>>>
type _AssertImageContent = Assert<Equal<
  operations['createImage']['responses'][201]['content']['application/json'],
  components['schemas']['Image']
>>

// Image 元数据：不再携带 node_images，仅 id/name/default_user/download_url/created_at
// （节点存在状态由 nodes-status 实时扫描与 ImageZoneItem.nodes 承载）
type _AssertImageFields = Assert<Equal<keyof components['schemas']['Image'],
  'id' | 'name' | 'default_user' | 'download_url' | 'created_at'>>

// NodeImageStatus：节点存在状态（downloaded 布尔，volid 可选）
type _AssertNodeImageStatusFields = Assert<Equal<keyof components['schemas']['NodeImageStatus'],
  'node_id' | 'node_name' | 'pve_name' | 'downloaded' | 'volid'>>
type _AssertNodeImageStatusDownloaded = Assert<Equal<components['schemas']['NodeImageStatus']['downloaded'], boolean>>

// ImageZoneItem：image 元数据 + 该区域各启用节点存在状态
type _AssertImageZoneItemFields = Assert<Equal<keyof components['schemas']['ImageZoneItem'], 'image' | 'nodes'>>
type _AssertImageZoneItemNodes = Assert<Equal<components['schemas']['ImageZoneItem']['nodes'],
  components['schemas']['NodeImageStatus'][]>>

// ImageDownloadRequest：node_ids 与 zone_id 互斥二选一（均可选）
type _AssertImageDownloadReqFields = Assert<Equal<keyof components['schemas']['ImageDownloadRequest'],
  'node_ids' | 'zone_id'>>
type _AssertImageDownloadReqOptional = Assert<Equal<OptionalKeys<components['schemas']['ImageDownloadRequest']>,
  'node_ids' | 'zone_id'>>

// ImageOperation：下载操作记录（result 为 running/success/failed 终态流转）
type _AssertImageOperationFields = Assert<Equal<keyof components['schemas']['ImageOperation'],
  'id' | 'image_id' | 'node_id' | 'action' | 'result' | 'error_message' | 'upid' | 'created_at' | 'updated_at'>>
type _AssertImageOperationResult = Assert<Equal<components['schemas']['ImageOperation']['result'],
  'running' | 'success' | 'failed'>>

// getImage：200 响应体为 Image
type _AssertGetImageContent = Assert<Equal<
  operations['getImage']['responses'][200]['content']['application/json'],
  components['schemas']['Image']
>>
type _AssertGetImageFn = Assert<Equal<ReturnType<typeof getImage>, Promise<ApiResponse<Image>>>>

// listImageNodeStatus：200 响应体为 NodeImageStatus[]
type _AssertListNodeStatusContent = Assert<Equal<
  operations['listImageNodeStatus']['responses'][200]['content']['application/json'],
  components['schemas']['NodeImageStatus'][]
>>
type _AssertListNodeStatusFn = Assert<Equal<ReturnType<typeof getImageNodeStatus>,
  Promise<ApiResponse<NodeImageStatus[]>>>>

// downloadImage：202 响应体为 ImageOperation[] 且带 Location 头（指向操作历史端点）
type _AssertDownload202Content = Assert<Equal<
  operations['downloadImage']['responses'][202]['content']['application/json'],
  components['schemas']['ImageOperation'][]
>>
type _AssertDownload202LocationHeader = Assert<
  'Location' extends keyof operations['downloadImage']['responses'][202]['headers'] ? true : false
>
type _AssertDownloadFn = Assert<Equal<ReturnType<typeof downloadImage>,
  Promise<LocatedResponse<ImageOperation[]>>>>

// listImageOperations：200 响应体为 ImageOperation[]，带 X-Total-Count 分页头
type _AssertListImageOpsQuery = Assert<Equal<
  Exclude<operations['listImageOperations']['parameters']['query'], undefined>,
  Exclude<Parameters<typeof listImageOperations>[1], undefined>
>>
type _AssertListImageOpsBody = Assert<Equal<
  operations['listImageOperations']['responses'][200]['content']['application/json'],
  components['schemas']['ImageOperation'][]
>>
type _AssertListImageOpsFn = Assert<Equal<ReturnType<typeof listImageOperations>,
  Promise<ListResponse<ImageOperation[]>>>>
type _AssertListImageOpsTotalHeader = Assert<
  'X-Total-Count' extends keyof operations['listImageOperations']['responses'][200]['headers'] ? true : false
>

// createStorageType/updateStorageType：StorageTypeRequest 全字段必填（name/display_name/pve_storage）
type _AssertStorageTypeReq = Assert<Equal<keyof components['schemas']['StorageTypeRequest'],
  'name' | 'display_name' | 'pve_storage'>>
type _AssertStorageTypeReqRequired = Assert<Equal<RequiredKeys<components['schemas']['StorageTypeRequest']>,
  'name' | 'display_name' | 'pve_storage'>>
type _AssertCreateStorageTypeFn = Assert<Equal<Parameters<typeof createStorageType>[0],
  components['schemas']['StorageTypeRequest']>>
type _AssertUpdateStorageTypeFn = Assert<Equal<Parameters<typeof updateStorageType>[1],
  components['schemas']['StorageTypeRequest']>>
type _AssertCreateStorageTypeRes = Assert<Equal<ReturnType<typeof createStorageType>,
  Promise<LocatedResponse<StorageType>>>>
type _AssertUpdateStorageTypeRes = Assert<Equal<ReturnType<typeof updateStorageType>,
  Promise<ApiResponse<StorageType>>>>
type _AssertStorageTypeContent = Assert<Equal<
  operations['updateStorageType']['responses'][200]['content']['application/json'],
  components['schemas']['StorageType']
>>

// 列表端点 query 参数：limit/offset 可选；listImages 契约 query 另有可选 zone_id（区域过滤）。
// 无区域封装（listImages）不带 zone_id；区域过滤由 listImagesByZone（首位必填 zoneId 参数）承载，
// 两者共享 limit/offset 分页参数，与契约 query 的 limit/offset 类型一致
// （契约 query 类型为 `{...} | undefined`；封装函数参数带默认值故 Parameters 亦含 undefined，
//   两侧均 Exclude 后逐一比对）
type _AssertListZonesQuery = Assert<Equal<
  Exclude<operations['listZones']['parameters']['query'], undefined>,
  Exclude<Parameters<typeof listZones>[0], undefined>
>>
type _AssertListPoolsQuery = Assert<Equal<
  Exclude<operations['listPools']['parameters']['query'], undefined>,
  Exclude<Parameters<typeof listPools>[0], undefined>
>>
type _AssertListStorageTypesQuery = Assert<Equal<
  Exclude<operations['listStorageTypes']['parameters']['query'], undefined>,
  Exclude<Parameters<typeof listStorageTypes>[0], undefined>
>>
type _AssertListImagesQueryHasZoneId = Assert<
  'zone_id' extends keyof Exclude<operations['listImages']['parameters']['query'], undefined> ? true : false
>
type _AssertListImagesQuery = Assert<Equal<
  Exclude<operations['listImages']['parameters']['query'], undefined>,
  { zone_id?: components['parameters']['QueryZoneID'], limit?: components['parameters']['QueryLimit'], offset?: components['parameters']['QueryOffset'] }
>>
type _AssertListImagesFn = Assert<Equal<
  Exclude<Parameters<typeof listImages>[0], undefined>,
  { limit?: components['parameters']['QueryLimit'], offset?: components['parameters']['QueryOffset'] }
>>
type _AssertListImagesByZoneFn = Assert<Equal<
  Exclude<Parameters<typeof listImagesByZone>[1], undefined>,
  { limit?: components['parameters']['QueryLimit'], offset?: components['parameters']['QueryOffset'] }
>>
type _AssertListImagesByZoneZoneId = Assert<Equal<
  Parameters<typeof listImagesByZone>[0],
  components['parameters']['QueryZoneID']
>>

// 带 X-Total-Count 头的 4 个列表端点（zones/pools/storage-types/images）响应头断言
type _AssertListZonesTotalHeader = Assert<
  'X-Total-Count' extends keyof operations['listZones']['responses'][200]['headers'] ? true : false
>
type _AssertListPoolsTotalHeader = Assert<
  'X-Total-Count' extends keyof operations['listPools']['responses'][200]['headers'] ? true : false
>
type _AssertListStorageTypesTotalHeader = Assert<
  'X-Total-Count' extends keyof operations['listStorageTypes']['responses'][200]['headers'] ? true : false
>
type _AssertListImagesTotalHeader = Assert<
  'X-Total-Count' extends keyof operations['listImages']['responses'][200]['headers'] ? true : false
>

// getVM：200 响应体为 VMListItem，封装函数返回 ApiResponse<VMListItem>
type _AssertGetVMContent = Assert<Equal<
  operations['getVM']['responses'][200]['content']['application/json'],
  components['schemas']['VMListItem']
>>
type _AssertGetVMFn = Assert<Equal<ReturnType<typeof getVM>, Promise<ApiResponse<VMListItem>>>>

// startVM/stopVM/restartVM：202 响应体为 AcceptedResponse 且带 Location 头
type _AssertVM202Content = Assert<Equal<
  operations['startVM']['responses'][202]['content']['application/json'],
  components['schemas']['AcceptedResponse']
>>
type _AssertVM202LocationHeader = Assert<
  'Location' extends keyof operations['startVM']['responses'][202]['headers'] ? true : false
>
type _AssertStartVMFn = Assert<Equal<ReturnType<typeof startVM>,
  Promise<LocatedResponse<AcceptedResponse>>>>
type _AssertStopVMFn = Assert<Equal<ReturnType<typeof stopVM>,
  Promise<LocatedResponse<AcceptedResponse>>>>
type _AssertRestartVMFn = Assert<Equal<ReturnType<typeof restartVM>,
  Promise<LocatedResponse<AcceptedResponse>>>>

// importVM：ImportVMRequest 仅 zone_id/node_id/pve_vmid 必填（ip/name 可选）；
// 响应 201 为 VMListItem（含 PVE 实时透传字段）+ Location
type _AssertImportVMReq = Assert<Equal<keyof components['schemas']['ImportVMRequest'],
  'zone_id' | 'node_id' | 'pve_vmid' | 'ip' | 'name'>>
type _AssertImportVMReqRequired = Assert<Equal<RequiredKeys<components['schemas']['ImportVMRequest']>,
  'zone_id' | 'node_id' | 'pve_vmid'>>
type _AssertImportVMFn = Assert<Equal<Parameters<typeof importVM>[0], components['schemas']['ImportVMRequest']>>
type _AssertImportVMRes = Assert<Equal<ReturnType<typeof importVM>, Promise<LocatedResponse<VMListItem>>>>
type _AssertImportVMContent = Assert<Equal<
  operations['importVM']['responses'][201]['content']['application/json'],
  components['schemas']['VMListItem']
>>
type _AssertImportVMLocationHeader = Assert<
  'Location' extends keyof operations['importVM']['responses'][201]['headers'] ? true : false
>

// listVMOperations：id 支持数字本地行 id 与 ext- 合成标识（PathVMRef）；响应体为
// VMOperationsResponse 包装（operations），带 X-Total-Count 分页头；封装函数返回 ListResponse
type _AssertListOperationsQuery = Assert<Equal<
  Exclude<operations['listVMOperations']['parameters']['query'], undefined>,
  Exclude<Parameters<typeof listVMOperations>[1], undefined>
>>
type _AssertListOperationsBody = Assert<Equal<components['schemas']['VMOperationsResponse']['operations'],
  components['schemas']['VMOperation'][]>>
type _AssertListOperationsFn = Assert<Equal<ReturnType<typeof listVMOperations>,
  Promise<ListResponse<VMOperationsResponse>>>>
type _AssertListOperationsTotalHeader = Assert<
  'X-Total-Count' extends keyof operations['listVMOperations']['responses'][200]['headers'] ? true : false
>
// types.ts 别名与契约镜像一致（操作记录相关）
type _AssertVMOperationAlias = Assert<Equal<VMOperation, components['schemas']['VMOperation']>>
type _AssertVMOperationsResAlias = Assert<Equal<VMOperationsResponse, components['schemas']['VMOperationsResponse']>>
type _AssertVMOperationFields = Assert<Equal<keyof components['schemas']['VMOperation'],
  'id' | 'node_id' | 'pve_vmid' | 'action' | 'result' | 'error_message' | 'created_at'>>
// 生命周期/操作记录路径参数 PathVMRef 为 ext-{nodeID}-{vmid} 或数字 id 的字符串形态
type _AssertVMOperationIdRef = Assert<Equal<
  components['parameters']['PathVMRef'],
  string
>>

// types.ts 别名与契约镜像一致（镜像下载相关）
type _AssertImageZoneItemAlias = Assert<Equal<ImageZoneItem, components['schemas']['ImageZoneItem']>>
type _AssertNodeImageStatusAlias = Assert<Equal<NodeImageStatus, components['schemas']['NodeImageStatus']>>
type _AssertImageDownloadReqAlias = Assert<Equal<ImageDownloadRequest, components['schemas']['ImageDownloadRequest']>>
type _AssertImageOperationAlias = Assert<Equal<ImageOperation, components['schemas']['ImageOperation']>>

// 兜底引用：确保以上断言类型被程序包含（import type 已保证在类型图中）
export type {
  _Assert31Ops,
  _AssertCreateVMReq,
  _AssertCreateVMReqRequired,
  _AssertCreateVMFn,
  _AssertCreateVMRes,
  _AssertResizeKeys,
  _AssertResizeOptional,
  _AssertResizeFn,
  _AssertResizeRes,
  _AssertDestroy204NoBody,
  _AssertDestroyFn,
  _AssertNodeReq,
  _AssertNodeReqRequired,
  _AssertNodeResNoToken,
  _AssertNodeResTokenSet,
  _AssertCreateNodeFn,
  _AssertUpdateNodeFn,
  _AssertNodeRes,
  _AssertPoolReq,
  _AssertPoolReqRequired,
  _AssertCreatePoolFn,
  _AssertSetPoolNodes,
  _AssertSetPoolNodesFn,
  _AssertVMListBody,
  _AssertVMListWarnings,
  _AssertListVMsFn,
  _AssertListVMsTotalHeader,
  _AssertCreateVMLocationHeader,
  _AssertZoneReq,
  _AssertZoneReqRequired,
  _AssertCreateZoneFn,
  _AssertCreateZoneRes,
  _AssertZoneContent,
  _AssertImageReq,
  _AssertImageReqRequired,
  _AssertImageReqNoNodeImages,
  _AssertCreateImageFn,
  _AssertCreateImageRes,
  _AssertImageContent,
  _AssertImageFields,
  _AssertNodeImageStatusFields,
  _AssertNodeImageStatusDownloaded,
  _AssertImageZoneItemFields,
  _AssertImageZoneItemNodes,
  _AssertImageDownloadReqFields,
  _AssertImageDownloadReqOptional,
  _AssertImageOperationFields,
  _AssertImageOperationResult,
  _AssertGetImageContent,
  _AssertGetImageFn,
  _AssertListNodeStatusContent,
  _AssertListNodeStatusFn,
  _AssertDownload202Content,
  _AssertDownload202LocationHeader,
  _AssertDownloadFn,
  _AssertListImageOpsQuery,
  _AssertListImageOpsBody,
  _AssertListImageOpsFn,
  _AssertListImageOpsTotalHeader,
  _AssertStorageTypeReq,
  _AssertStorageTypeReqRequired,
  _AssertCreateStorageTypeFn,
  _AssertUpdateStorageTypeFn,
  _AssertCreateStorageTypeRes,
  _AssertUpdateStorageTypeRes,
  _AssertStorageTypeContent,
  _AssertListZonesQuery,
  _AssertListPoolsQuery,
  _AssertListStorageTypesQuery,
  _AssertListImagesQueryHasZoneId,
  _AssertListImagesQuery,
  _AssertListImagesFn,
  _AssertListImagesByZoneFn,
  _AssertListImagesByZoneZoneId,
  _AssertListZonesTotalHeader,
  _AssertListPoolsTotalHeader,
  _AssertListStorageTypesTotalHeader,
  _AssertListImagesTotalHeader,
  _AssertGetVMContent,
  _AssertGetVMFn,
  _AssertVM202Content,
  _AssertVM202LocationHeader,
  _AssertStartVMFn,
  _AssertStopVMFn,
  _AssertRestartVMFn,
  _AssertImportVMReq,
  _AssertImportVMReqRequired,
  _AssertImportVMFn,
  _AssertImportVMRes,
  _AssertImportVMContent,
  _AssertImportVMLocationHeader,
  _AssertListOperationsQuery,
  _AssertListOperationsBody,
  _AssertListOperationsFn,
  _AssertListOperationsTotalHeader,
  _AssertVMOperationAlias,
  _AssertVMOperationsResAlias,
  _AssertVMOperationFields,
  _AssertVMOperationIdRef,
  _AssertImageZoneItemAlias,
  _AssertNodeImageStatusAlias,
  _AssertImageDownloadReqAlias,
  _AssertImageOperationAlias
}
