## Context

现状（见 proposal.md - Why）：节点仅有配置管理 API（`GET /zones/:zone_id/nodes`、`PUT /nodes/:id`），无运行时状态；前端节点总览页只展示配置字段。PVE 客户端（`pve/client.go`）已有 `NewClient`（host/port/API 用户/token）与 `doJSON` 通用请求封装、`UpstreamError` 错误模型；service 层已有 `KindNodeUnavailable → 503 node_unavailable` 降级错误映射（`api/handlers/zone_handler.go`）；`repository.NodeRepository.GetNode` 可读取节点凭据。镜像服务（`service/image_service.go`）建立了"nodeRepo 取节点 → newClient 工厂注入 → 调 PVE"的可测模式，本设计沿用。

## Goals / Non-Goals

**Goals:**

- 新增只读端点 `GET /nodes/:id/status`，实时拉取 PVE 节点状态并聚合返回
- 节点总览页每行展示 CPU/内存使用率，详情页展示完整状态
- 全程遵守现有契约红线：openapi.yaml 双副本同步 + 前端 client 重新生成

**Non-Goals:**

- 不做节点状态落库/历史趋势（用户已确认实时拉取）
- 不做自动轮询与阈值告警
- 不修改节点 CRUD 行为与现有错误契约

## Decisions

### D1. 新端点：`GET /nodes/:id/status`

- 资源化设计：`/nodes` 集合 + `:id` 项 + `status` 子资源，符合"集合/项"层级，不引入动词路径。
- `:id` 是本地 `pve_nodes.id`（与 `PUT /nodes/:id` 一致），服务层先 `GetNode` 解析出 host/port/凭据与 PVE 节点名，再调 PVE 的 `GET /nodes/{pve_name}/status`。PVE 节点名用业务记录的 `PveName`（为空时按现有语义沿用 `Name`，与 `pve/client` 其他调用方一致）。
- 响应 = 现有 `nodeResponse` 全部配置字段（平铺）+ 嵌套 `status` 对象（运行时数据），前端详情页可复用列表页的类型定义。
- 错误：节点不存在 → `404 not_found`（复用 `mapServiceErrorExtended` 默认分支）；PVE 不可达/令牌无效/超时 → `503 node_unavailable`（复用现有 `service.KindNodeUnavailable` 映射，错误消息对外脱敏）。
- 备选：`GET /nodes/:id` 直接扩展返回状态 —— 被否决：GET 单项资源会变成"配置+实时状态"混合语义且每次请求都扇出 PVE 调用，破坏现有调用方（`PUT` 响应、前端列表）的稳定负载；独立 `status` 子资源语义清晰、可独立降级。

### D2. PVE 客户端：新增 `pve/status.go`

- `NodeStatus(ctx)` → `GET /nodes/{node}/status`，双版本兼容（生产环境为 PVE 9.1.1，已实测）：PVE 7/8 字段 `cpu`（0-1 小数）、`cpus/maxcpu`、`mem/maxmem`（字节）、`rootfs/maxrootfs`（字节，rootfs 兼容对象与裸数字双格式）、`status`、`uptime`（秒）、`version`、`kversion`、`loadavg`；PVE 9 新增 `cpuinfo`（cpus/cores/sockets/model）、`memory`（total/used/free/available）对象与 `pveversion`，并移除 `status/node/version/mem/maxmem/cpus/maxcpu/maxrootfs` 旧字段。取值回退链在 handler 层固定：cores 取 cpuinfo.cpus → cpus → maxcpu；内存取 memory 对象 → maxmem/mem；版本取 pveversion → version；status 缺失（PVE 9）时 service 层补 "online"。
- `NodeNetwork(ctx)` → `GET /nodes/{node}/network`，结构化接口列表：`iface`、`type`、`address`、`active` 等；active 用自定义 `PveBool` 兼容布尔与数字 1/0（PVE 9 实测为数字）。
- `NodeNetIO(ctx)` → `GET /nodes/{node}/rrddata?timeframe=hour`，节点级网络吞吐（bytes/s），取数组最后一个点的 `netin/netout` 作为当前值；数组为空返回全零容错。
- 三个调用分两组：核心组（status/network）并发执行（errgroup 或手写 goroutine），任一失败即 context.WithCancel 取消另一 in-flight 请求并整体降级（503 不等最慢请求）；rrddata（增强字段）在核心组成功后单独串行调用。单个 PVE 请求超时沿用客户端 30s 默认。
- 备选：只调 `/status` 不取网络 —— 被否决：用户明确要求网络信息（接口 + 吞吐）。
- 备选（原决策，已推翻）：`NodeNetStats` → `GET /nodes/{node}/netstat` 解析 ifconfig 文本 —— PVE 9 实测 netstat 只返回 VM 网络设备计数器（`dev/vmid/in/out`），无物理网卡流量，节点级流量改用 rrddata；接口级 rx/tx 随之从响应中移除。

### D3. Service：`service/node_status_service.go`

- 新增 `NodeStatusService`，依赖 `NodeRepository`（取节点凭据）与可注入的 `newClient` 工厂（同 ImageService 模式，测试注入假服务器）。
- `GetStatus(ctx, nodeID)` 组装 D1/D2：GetNode → 构建 client → 核心组并发拉取 status/network → 串行拉取 rrddata → 聚合为响应结构。**rrddata（网络吞吐）为增强字段，需 Sys.Audit 权限，权限不足或失败时降级为 0 值（`&pve.NodeIO{}`），不整体 503；仅 status/network 失败才整体降级**（设计决策，见 M1 复审项）。PVE 9 的 status 无 status 字段时补 "online"（请求成功即在线）。
- 不新增 service 错误类型：`KindNodeUnavailable` 已存在，错误映射零改动。

### D4. Handler 与路由

- 新增 `api/handlers/node_status_handler.go`：`RegisterNodeStatusRoutes(nodesGroup, svc)` 挂载 `GET /:id/status`。
- `api/router.go` 在现有 `nodesGroup`（`/nodes`）上追加注册，复用现有 `ZoneHandler` 的 handler 封装模式（`Handler(...)` + `mapServiceErrorExtended`）。
- 错误码契约：新增响应字段不新增错误码，`docs/api-errors.md` 无需变更。

### D5. 前端

- 总览页 `web/app/pages/nodes.vue`：行内新增「CPU」「内存」列；进入后并行请求各节点 `status`（结果存 `Map<nodeID, status>`），不可达节点该列展示降级徽标与提示，顶部加手动刷新按钮（不自动轮询）。详情入口沿用现有「管理」按钮样式新增「详情」入口。
- 新增详情页 `web/app/pages/zones/[zoneId]/nodes/[nodeId].vue`：调用 `GET /nodes/:id/status` 一次性取配置+状态；配置卡片（复用列表字段）+ 状态卡片（CPU 核数/使用率/负载、内存总量/已用/可用、根分区、网络接口表、PVE 版本/内核/在线时长/在线状态）；手动刷新按钮；加载失败展示 `AppErrorAlert`（含降级提示）。
- 路由：详情页挂在 `zones/[zoneId]/nodes/[nodeId].vue`，与现有 `zones/[zoneId]/nodes`（区域节点管理页）路径兼容不冲突。
- 前端 API：契约生成 client 后新增 status 调用函数，`npm run api:check` 保证生成物与提交版本一致。

### D6. 契约与测试

- `docs/openapi.yaml` 新增 `/nodes/{id}/status` 路径与 `NodeStatusResponse` schema（含嵌套 `status` 对象与网络接口数组），operationId 完整；同步复制到 `api/swagger/openapi.yaml`，双副本字节一致（PR checklist 校验）。
- 单元测试：`pve/status_test.go`（假 HTTP 服务器覆盖 status PVE 7/8/9 双格式、network active 布尔/数字/缺失、rrddata 解析与错误）、`service/node_status_service_test.go`（节点不存在、PVE 不可达降级、聚合成功、PVE 9 status 默认 online）、`api/handlers/node_status_handler_test.go`（200/404/503、回退链与脱敏）。
- `e2e/` 保持通过：PVE 客户端仅新增方法，不改既有方法签名；fake PVE 服务器扩展新增端点路由（如 e2e 需要）。

## Risks / Trade-offs

- [PVE 版本差异（7/8/9 字段不同：cpuinfo/memory/pveversion 对象 vs 旧字段）] → 客户端结构体双格式字段并存 + handler 层固定回退链（PVE 9 优先、7/8 兜底），缺失字段按零值容错，已用真实 PVE 9.1.1 响应验证。
- [rrddata 为历史时间序列，语义是"最近采样点吞吐"而非瞬时计数器] → 取最后一个点（最近采样）作为当前吞吐，单位 bytes/s；数组为空时返回全零，不整体降级。
- [总览页对 N 个节点扇出 N 个实时请求，节点多时慢或打爆 PVE] → 前端并行请求但仅手动刷新、不自动轮询（spec 已约束 ≤10s 频率，实际完全不轮询）；每个 PVE 请求有 30s 超时兜底。
- [每次请求实时拉取增加 PVE 压力] → 用户已确认实时拉取方案；无缓存符合"状态实时性"spec 要求。
- [`loadavg` 为字符串数组（PVE 原文）] → 透传字符串，不强行数值化，避免精度损失与解析错误。

## Migration Plan

- 纯新增：新端点、新 service、新前端页面；无 DB 变更、无既有行为修改，随分支正常合并发布。
- 回滚：移除路由注册与详情页即可，不影响节点 CRUD 与既有页面。

## Open Questions

无。
