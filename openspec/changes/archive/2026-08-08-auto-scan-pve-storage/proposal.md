## Why

当前存储类型（storage_types）靠管理员手工登记 name/display_name/pve_storage 三元组完成对 PVE 存储的映射，属于纯人肉同步：PVE 侧新增/删除/变更存储后 Spark 无感知，映射错了只能人工发现。镜像存在性已经采用"PVE 为权威源、实时扫描"模式，存储理应同样自动化。

## What Changes

- 存储类型由"手动登记"改为"扫描同步"：从 PVE `GET /storage` 自动发现存储并落库，按 zone 隔离（一个 zone 对应一个 PVE 集群）。
- storage_types 表结构改造：
  - 新增 `zone_id`（唯一约束改为 `(zone_id, pve_storage)`）、`enabled`（默认 true）、`type`、`content`（快照）。
  - `name` 改为可空，展示时 fallback 到 `pve_storage`；**删除 `display_name` 字段**（BREAKING）。
  - `pve_storage` 成为只读字段，由扫描权威填充，不再可手改。
- 新增扫描能力：手动触发 `POST /storage-types/scan?zone_id=X` 返回 `{created, updated, deleted, skipped}` 摘要；节点注册成功时自动触发一次该 zone 的扫描。扫描同步语义：PVE 消失的存储同步删除（被本地 VM 引用导致外键冲突的行跳过并计入 skipped）。
- 存储能力完整体现：API 响应派生 PVE 全 content 枚举布尔（images/iso/backup/vztmpl/rootdir/snippets）及 `can_download_image`（dir 类型），前端展示"能放 ISO 还是硬盘"。
- 移除 `POST /storage-types` 手动创建（BREAKING）；`PUT` 仅允许修改 `name`（可空）与 `enabled`。
- 创建 VM 新增校验：所选存储必须属于该 zone、`enabled` 且支持 `images`（否则拒绝）；调度节点时按所选存储的节点挂载过滤候选，无候选时返回存储不可用错误。
- 节点挂载快照：扫描同步 PVE 存储的 nodes 字段（该存储挂在哪些节点上，空 = 不限制）；管理页按节点分组展示存储，创建 VM 时调度联动（磁盘只落到挂载了所选存储的节点）。
- 存量数据迁移：zones 仅一行时自动归入该 zone，多行时 migration 失败并提示人工处理。

## Capabilities

### New Capabilities

- `storage-scan`: 自动扫描 PVE 存储并同步到本地（触发方式、同步语义、能力派生、删除兜底）

### Modified Capabilities

- `storage-types`: 需求整体从"手动映射"改为"扫描同步 + 启用开关 + 能力展示"；`display_name` 移除、`name` 可空、`pve_storage` 只读
- `vm-lifecycle`: 创建虚拟机新增存储可用性校验（enabled + 支持 images）

## Impact

- **API**：`docs/openapi.yaml` 与 `api/swagger/openapi.yaml` 双副本同步；`/storage-types` 响应结构变更、`POST` 移除、`PUT` 字段变更、新增 `POST /storage-types/scan`
- **数据库**：新增 migration（storage_types 加列/改约束/去 display_name）；旧行 zone 归属策略
- **PVE 客户端**：`pve/storage.go` 新增 `GET /storage` 封装（ListStorage）
- **Service/Repository**：`StorageTypeService` 重写为扫描同步 + 元数据编辑；`VMService.CreateVM` 加存储校验
- **前端**：`web/app/api/types.ts`、`client.ts`、`pages/storage-types.vue`（管理页改为"扫描 + 开关"形态）、`pages/vms.vue`（展示 fallback `name || pve_storage`，删除 display_name 引用）
- **测试**：e2e 依赖 fake PVE 服务器新增 `/storage` 应答；repo pg 测试与 service 单测同步更新
