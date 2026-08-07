## Purpose

为运维人员提供节点的实时运行状态监控：从 PVE 实时拉取节点的 CPU、内存、磁盘、网络与集群信息，经 API 暴露并支撑前端列表简略展示与详情页深入查看。

## Requirements

### Requirement: 节点实时状态查询

系统 SHALL 提供 `GET /nodes/:id/status` 端点，实时从 PVE 拉取指定节点的运行状态（CPU 核数与使用率、内存总量/已用/可用、根分区磁盘容量与使用、网络接口列表、PVE 版本与内核、在线时长与在线状态），并与该节点的配置信息一并返回。状态为 PVE 实时透传，MUST NOT 落库。节点不存在时返回 404；节点存在但 PVE 不可达或 API 令牌无效时，系统 SHALL 返回 `node_unavailable` 降级错误（HTTP 503），MUST NOT 伪造或返回过期状态。

#### Scenario: 节点在线查询成功

- **WHEN** 运维人员请求一个存在且 PVE 可达的节点的状态
- **THEN** 系统返回 200，负载包含该节点配置信息与 PVE 实时返回的 CPU/内存/磁盘/网络/集群信息

#### Scenario: 节点不存在

- **WHEN** 运维人员请求一个不存在的节点 id
- **THEN** 系统返回 404 与 not_found 错误

#### Scenario: 节点 PVE 不可达

- **WHEN** 运维人员请求一个存在但其 PVE 节点不可达（网络失败、超时或 API 令牌无效）的节点状态
- **THEN** 系统返回 503 与 `node_unavailable` 降级错误，错误信息不泄露内部细节

### Requirement: 节点状态实时性

节点状态数据 SHALL 在每次请求时实时从 PVE 拉取，响应中的各项状态字段直接反映 PVE 当前值；系统 MUST NOT 缓存或持久化节点状态作为响应来源。

#### Scenario: 状态跟随 PVE 变化

- **WHEN** 运维人员先后两次请求同一节点的状态，期间 PVE 侧状态发生变化
- **THEN** 第二次响应反映 PVE 的最新值
