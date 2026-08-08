/**
 * API client 统一出口：页面通过 `~/api` 导入全部端点函数、类型与 ApiError。
 * 类型由 docs/openapi.yaml 经 openapi-typescript 生成（见 package.json 的 api:gen）。
 */
export * from './client'
export * from './types'
export * from './auth'
export * from './healthz'
export * from './zones'
export * from './nodes'
export * from './pools'
export * from './storage-types'
export * from './images'
export * from './vms'
