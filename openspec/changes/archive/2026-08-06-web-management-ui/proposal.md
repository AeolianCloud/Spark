## Why

Spark 后端 API 已覆盖完整管理面（可用区/节点/IP 池/存储类型/镜像/VM 全生命周期），但所有操作目前只能通过 curl 或 Swagger UI 完成，没有可视化界面。需要一个内部平台管理面，让平台运维人员能直接管理基础设施与虚拟机。

## What Changes

- 新增 `web/` 子目录，引入 Nuxt 4 + Nuxt UI v4 前端框架（首个前端依赖，独立于后端 Go 生态）
- 实现全功能管理面：Dashboard 概览、Zones/Nodes/IP Pools/Storage Types/Images 的 CRUD、VMs 列表（分页）/详情/创建/启停/调整大小/销毁
- 前端 API client 从 `docs/openapi.yaml` 契约生成（contract-first，契约是前后端唯一的事实来源）
- 第一批**不加鉴权**（已知权衡：任何能访问界面的人都拥有全部 VM 的操作权，鉴权作为后续独立变更）

## Capabilities

### New Capabilities

- `web-management-ui`: 面向内部平台运维的管理 Web 界面，覆盖现有后端全部管理端点的读写操作

### Modified Capabilities

<!-- 无：本变更不改动任何后端 API 行为 -->

## Impact

- 新增 `web/` 目录：Nuxt 应用（package.json、pages、components、composables 等），CI 需引入 node 构建步骤
- 新增 npm 依赖与 lockfile（node 生态首次进入仓库）
- 后端 API 无变更：第一批复用现有 25 个端点，无鉴权、无路由变更
- 端到端联调：前端 dev proxy 指向本地后端；e2e 测试保持不受影响
