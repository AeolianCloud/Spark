## Why

当前节点页面仅展示配置信息（名称、主机、端口、令牌状态），运维人员无法在界面看到节点的 CPU、内存、磁盘、网络等实时运行状态；如需查看节点健康状况，只能手工调用 PVE API。需要提供节点状态监控能力：列表简略展示，详情深入查看。

## What Changes

- 新增 PVE 客户端方法：拉取单节点实时状态（`GET /nodes/{node}/status`）与网络统计（`GET /nodes/{node}/netstat`），以及节点上的资源使用概览。
- 新增后端 API `GET /nodes/:id/status`：实时从 PVE 拉取指定节点的状态（CPU/内存/磁盘/网络/集群信息），节点不可达时按现有降级契约返回明确错误；该端点属于只读查询，不落库。
- 节点总览页（`/nodes`）每行新增简略状态展示（CPU、内存使用率），数据来源于新状态 API；PVE 不可达时展示降级提示而非伪造状态。
- 新增节点详情独立路由页（`/zones/:zoneId/nodes/:nodeId`），展示节点的完整配置信息与实时运行状态（CPU 核数/使用率、内存总量/已用/可用、根分区与磁盘、网络流量、PVE 版本与集群信息）。
- 同步更新 `docs/openapi.yaml`（权威源）与 `api/swagger/openapi.yaml`（挂载副本），并重新生成前端 API client。

## Capabilities

### New Capabilities

- `node-monitor`: 节点实时运行状态的监控能力：从 PVE 实时拉取节点 CPU/内存/磁盘/网络与集群信息，经 `GET /nodes/:id/status` 暴露，节点不可达时返回降级错误。

### Modified Capabilities

- `web-management-ui`: 节点列表页由纯配置表格扩展为带 CPU/内存简略状态的展示，并新增节点详情页（配置 + 实时状态）。

## Impact

- 新增代码：`pve/status.go`（PVE 节点状态客户端）、`service/node_status_service.go`（状态查询服务）、`api/handlers/node_status_handler.go`（新端点）；前端 `web/app/pages/nodes.vue` 改造、`web/app/pages/zones/[zoneId]/nodes/[nodeId].vue` 新增。
- 修改文件：`api/router.go` 挂载新端点、`docs/openapi.yaml` 与 `api/swagger/openapi.yaml` 双副本、前端生成 client。
- 复用现有能力：`pve.NewClient`（host/port/token 凭据构建）、`repository.NodeRepository.GetNode`（节点凭据读取）、现有降级错误码与错误映射。
- 无数据库 schema 变更，无新增依赖。
