## Context

现状：`storage_types` 表为纯手工元数据（name/display_name/pve_storage 三元组，migration 0002），管理员经 POST /storage-types 登记，PVE 侧变更无感知。`vms.storage_type_id` 外键引用该表（0002），0007 已将其改为可空但 FK 保留。镜像存在性已确立"PVE 为权威源、实时扫描"模式（CONTEXT.md），存储扫描是同一哲学的延伸。PVE 客户端（pve/storage.go）目前只有 ListStorageContent 与 DownloadURL，尚无 GET /storage 封装。PVE 的 GET /storage 为集群级 cfs 配置接口，任一节点凭据均可调用，返回每存储的 storage/type/content/shared/nodes 字段。

## Goals / Non-Goals

**Goals:**
- 存储类型自动同步：手动触发 + 节点注册自动触发，免手工登记 pve_storage
- 能力完整体现：content 快照 + PVE 全枚举派生布尔 + can_download_image（dir 类型）
- 管理员保留最小控制权：业务名（可空，fallback pve_storage）+ enabled 开关
- zone 隔离：一个 zone 一个集群，存储按 zone 归属，`(zone_id, pve_storage)` 唯一
- VM 创建两道闸：enabled + 支持 images

**Non-Goals:**
- 不做定时任务自动刷新（手动 + 节点注册触发足够，见 Risks）
- 不修改 PVE 侧存储配置（增删改/重命名一律在 PVE 管理台操作，扫描被动感知）
- 不支持一个 zone 跨多个集群的部署形态（见 Risks）
- 不引入新的调度依赖（无 cron 框架，沿用现有 goroutine/请求路径）

## Decisions

### D1. 数据源与节点选择：集群级 GET /storage + zone 内首个可达启用节点

调用 `GET /storage`（集群级）而非逐节点 `GET /nodes/{node}/storage`——同一集群各节点看到同一份 cfs 配置，一次调用即可。扫描时从 zone 的启用节点中选首个可达节点（复用 zone_service 的 reachable 探测思路），全部不可达则报 node_unavailable 错误，**不产生部分同步**（避免半个 zone 的存储被误删）。

- 备选：逐节点聚合 → 无必要（集群级配置一致），且多节点结果冲突时反而引入歧义。

### D2. 扫描结果落库 + 手动刷新，查询不实时打 PVE

content/type 属低频变更配置，落库快照 + 显式刷新即可，与镜像存在性的"每次查询实时判定"不同——镜像判定是轻量存在性检查，存储同步是表级全量对齐，实时查询每个 GET /storage-types 都打 PVE 会引入网络抖动与节点依赖。

- 备选：每次查询实时合并 → 查询依赖节点可达性，列表页抖动；缓存 TTL → 增加复杂度，收益低。

### D3. 同步语义：upsert 尊重人工状态，删除遇 FK 跳过

- 匹配键 `(zone_id, pve_storage)`。新建：enabled=true、name=NULL。更新：仅覆盖 `type`/`content`，**不触碰** `name`/`enabled`。删除：PVE 消失的行调用现有 Delete（复用其 ErrInUse 语义），被 vms 引用的行跳过并计入摘要 skipped。同步为**逐条幂等执行**（upsert/delete 各自独立事务，无单事务）：中途 DB 故障会留下已提交的部分修改，由下一次扫描自愈收敛；并发的两次扫描不会损坏数据，仅摘要计数可能失真（已删行被计为 skipped）。
- 摘要 `{created, updated, deleted, skipped}` 作为扫描响应与日志结构。

### D4. 模型与 API：砍 display_name，name 可空，能力嵌套对象

- 表/模型删除 `display_name`（唯一消费方 web/app/pages/vms.vue:288 的 `${display_name}（${name}）` 标签改为 `name || pve_storage`）。
- `name` 可空；展示语义 `name || pve_storage`，由前端处理，服务端不做合成字段。
- 响应新增 `zone_id`、`enabled`、`type`、`content` 及嵌套 `capabilities`（images/iso/backup/vztmpl/rootdir/snippets 六枚举 + can_download_image=type==dir）。
- PUT 仅接收 `name`（允许空串置空）与 `enabled`；pve_storage 不再可写。
- BREAKING：移除 POST /storage-types。

### D5. VM 创建校验：两道闸在 CreateVM 同步路径

现有 CreateVM 已 `storageRepo.Get(id)`（vm_service.go:362），在取回后追加三道闸：`ZoneID != req.ZoneID → not_found`（与 IP 池的 zone 归属校验语义一致，防止跨 zone 引用把存储名下发给错误集群；优先于后两道 400 闸）；`Enabled != true → bad_request("storage type is disabled")`；`Content 不含 images → bad_request("storage type cannot store VM disks")`。校验在异步供给链之前，错误直接返回调用方。权限粒度（经审查修正）：/storage-types 的**读操作**（GET 列表/详情）仅 requireAuth——`storage_type_id` 是 user 创建 VM 的必填字段，存储列表与 /images、/ip-pools 同属 user 创建流程的参考数据；**写操作**（POST /scan、PUT/DELETE）挂 `RequireAdmin`——scan/update/delete 会改变全局可选存储，属管理面（写侧比 images/ip-pools 更严格）。

### D6. 触发：节点注册成功后直接调用扫描

node_service 创建成功分支直接调用 StorageTypeService.SyncZone(zoneID)（进程内同步调用，失败仅记日志不阻断节点注册——节点注册是主操作，扫描是副作用）。手动路径为 POST /storage-types/scan?zone_id=X，返回摘要。

### D7. 迁移策略：单 zone 自动归入，多 zone 失败

migration 0011 用 PL/pgSQL 块：`zones` 行数为 1 → `UPDATE storage_types SET zone_id=<该zone>`，随后加 NOT NULL 约束与 `(zone_id, pve_storage)` 唯一索引；多 zone 且 storage_types 非空 → `RAISE EXCEPTION` 中止迁移并提示人工处理。display_name 直接 DROP COLUMN（无独立消费价值）；name DROP NOT NULL；新增 enabled/type/content 列（enabled 默认 true，type/content 可空，NULL 表示尚未扫描的存量行）。

### D8. 节点挂载快照与调度联动

- **落库**：新建 migration 0012（0011 已审查并应用不可改），storage_types 加 `nodes TEXT NOT NULL DEFAULT ''`，存 PVE 原文（逗号分隔节点名）；`''` 语义为"不限制节点"。UpsertByZonePveStorage 追加 nodes 参数（PVEStorage.Nodes 已解析为 []string，join 后落库），仅扫描可写、同 name/enabled 一样不受 Update 影响。API 响应 `nodes: []string`（split），契约注明空数组 = 所有节点可用。
- **调度联动**（CreateVM 的 selectPoolAndNode 链）：在 poolCandidates（池白名单 ∩ zone 启用节点）之后、镜像存在性扫描之前插入存储挂载过滤——`storage.nodes` 非空时排除节点名不在其中的候选，减少无效的镜像扫描调用。过滤后无候选 → 新错误 `storage_not_available_in_zone`（与 image_not_available_in_zone 同构，bad_request 类）。
- **展示**：管理页按节点分组（每个节点列出挂载存储，nodes 为空的归"所有节点"组）；创建 VM 时存储下拉展示挂载节点提示。两者均为前端实现，后端只保证 nodes 快照与调度一致性。
- 备选：nodes 存 TEXT[]（pgx 原生支持）→ 库内结构更规范，但 PVE 原文就是逗号分隔字符串，TEXT + split 与既有列风格一致、迁移与测试更简单，取舍后选 TEXT。

## Risks / Trade-offs

- [content 快照可能过期] → 手动刷新兜底 + 节点注册自动扫描；不做定时任务的代价是 PVE 侧改 content 后需手动刷新一次（低频操作，可接受）
- [nodes 快照可能过期] → 与 content 同源同策略：重扫即更新；过期分两个方向表述：「PVE 新增挂载但快照未更新」→ 该节点暂时不被选（保守，仅调度延迟，无数据面风险）；「PVE 已摘挂但快照未更新」→ 调度可能落到已摘挂节点，磁盘供给在异步链失败（provision_error），重扫后自愈（快照更新后再次创建即可），不会把磁盘发到未挂载的节点（PVE 创建被拒绝）
- [zone 被误配为跨集群时扫描以所选节点为准] → 领域约定"一个 zone 一个集群"写入 CONTEXT.md 与 API 文档；扫描结果天然反映所选节点集群
- [被 VM 引用的消失存储滞留（skipped）] → 摘要与日志暴露 skipped 计数；VM 清理（Destroy）后再次扫描即删除，无需人工 SQL
- [多 zone 存量数据迁移中止] → migration 明确报错提示，管理员人工归置后重跑；开发期 zones 通常单行，路径极少触发
- [节点注册扫描失败静默] → 日志记录 + 手动刷新兜底，不阻断注册主流程

## Migration Plan

1. 数据库 migration 0011（见 D7）+ 0012（见 D8：storage_types 加 nodes 列）
2. PVE 客户端新增 ListStorage（GET /storage）与单测
3. Service/Repository 重写 storage 领域（SyncZone/update/delete 语义、nodes 快照），CreateVM 加校验闸与存储挂载过滤
4. API：router 增删路由、handler 重写、`docs/openapi.yaml` 与 `api/swagger/openapi.yaml` 双副本同步、错误码文档（docs/api-errors.md）核对
5. 前端：api/types.ts、client.ts 重生成（npm run api:check 为零 diff）、storage-types.vue 改"扫描+开关+按节点分组"形态、vms.vue 标签 fallback 与挂载提示
6. 测试：e2e fake PVE 服务器新增 /storage 应答与扫描场景；repo pg 测试、service 单测同步
7. 环境变量红线核对：本次无新增 SPARK_* 变量

回滚：migration 可逆性——display_name 列与 name NOT NULL 的恢复需反向 migration；鉴于 BREAKING 变更（API 移除 POST），发布前在开发/测试环境完整验证，生产部署顺序为先 DB 后应用。

## Open Questions

无（探索阶段已闭合：删除语义、name/display_name、迁移策略、触发方式均已确认）。
