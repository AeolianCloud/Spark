# Design: 全部 PVE 虚拟机可见、可控制、有标识

## Context

动机见 proposal.md Why。现状要点（探索结论）：

- `GET /vms` 为"本地 SQL 分页 + 每节点 PVE 全量摘要 join"，PVE 有而本地无的 VM 被刻意跳过（`service/vm_query.go:70`），只存在于 `GET /vms/unmanaged` 候选接口
- 生命周期操作（start/stop/reboot/destroy）的 PVE 调用只依赖 `node + vmid`，不依赖 cloudinit 与本地字段；唯一门禁是 service 层 `vmAndNode` 要求本地行存在（`vm_service.go:618-634`）
- `vms` 表无用户概念、无来源字段；`ip_id` 本就允许 NULL（迁移 0002），销毁时按 `ips.vm_id` 释放
- 前端 VM 列表使用 limit/offset 分页 + X-Total-Count（`web/app/pages/vms.vue:91-101`）
- 每节点 PVE 列表接口（`GET /nodes/{node}/qemu`）摘要自带 name/cpus/maxmem/maxdisk/status

## Goals / Non-Goals

Goals:
- 列表返回节点上全部 VM（含外部），三类来源标识稳定可用
- 外部 VM 无需认领即可 start/stop/reboot/destroy
- 所有生命周期操作落审计记录（user_id 预留可空）
- 节点故障时该节点 VM 隐藏 + 明确标记

Non-Goals:
- 用户体系、认证（单独提案，本设计仅为操作记录表预留 `user_id` 可空列）
- 精简 vms 表字段（随用户体系提案一起）
- 不引入状态缓存（节点故障即隐藏，无兜底展示）

## Decisions

### D1: 列表合并改为"PVE 全量 + 本地簿记 join"后内存排序分页

现状是 SQL LIMIT/OFFSET 作用于本地行，外部 VM 无法混入。改为：

```
1. 每启用节点并行 1 次 PVE 全量摘要（现有调用不变）
2. 本地 vms 表全量行（取消 SQL 分页，行数小）按 node_id+pve_vmid 建索引
3. join 出三类条目：
   ├─ 本地行且 PVE 有    → 本地元数据 + 实时状态（id=数字）
   ├─ 本地行但 PVE 无    → 保留 creating/failed 语义（现有 D5 逻辑不变）
   └─ PVE 有但本地无     → external 条目（id=合成标识，见 D2）
4. 统一列表按 (node_id, pve_vmid) 排序后内存切片分页
5. X-Total-Count = 合并后条目总数
```

替代方案：仅把外部 VM 追加到当前页尾部（分页语义混乱、翻页重复）、SQL 层 UNION（跨库不可行）。选择内存分页的理由：单节点 VM 数十台、节点数个，每请求全量合并开销可忽略，且外部 VM 的实时可见性要求天然排斥"仅本地行分页"。

### D2: external 条目使用合成标识 `ext:{nodeID}:{vmid}`

- 本地行条目 `id` 保持数字（现有路由、乐观锁、详情页全部兼容）
- external 条目 `id` 为 `ext-{nodeID}-{vmid}`，`uuid` 返回空、`created_at` 返回空
- VMService 生命周期入口按标识前缀路由：数字 → 现有本地行路径；`ext-` → 解析 node+vmid 直调 PVE
- 列表按 `(node_id, pve_vmid)` 排序保证翻页稳定，避免 external 条目因 PVE 顺序漂移导致翻页重复/漏项

### D3: 来源标识 = 实时差集 + 持久化列

- `external`：PVE 有而本地无 → 实时判定，不落库（spec 明确）
- `spark_created` / `claimed`：依赖 vms 表新增 `source` 列（默认 `spark_created`；认领时写 `claimed`）
- 迁移 0008 对存量已导入行（`image_id IS NULL`）回填 `claimed`，其余默认 `spark_created`
- 相比"用 image_id 是否为空推断"：显式列不受未来创建链路调整影响

### D4: 生命周期操作放开本地行强校验

- `vmAndNode` 改为：数字 id → 现有路径；`ext-` 前缀 → 反查 zone/node（按 nodeID）并校验 pve_vmid 在 PVE 存在（复用 ListVMs 索引或直接 GetVMConfig 探测）
- destroy 外部 VM：PVE destroy + 无本地行/IP 可释放，操作记录照写
- 本地行的 destroy 流程（IP 释放、行删除）保持不变
- PVE 404 → `vm_not_ready` 语义保留

### D5: 操作记录表 `vm_operations` + 受理后同步写

```
id BIGSERIAL PK
node_id BIGINT NOT NULL REFERENCES pve_nodes(id)
pve_vmid BIGINT NOT NULL
action TEXT NOT NULL            -- start/stop/reboot/destroy
result TEXT NOT NULL            -- accepted / failed
error_message TEXT NOT NULL DEFAULT ''
user_id BIGINT NULL             -- 预留，用户体系提案后启用
created_at TIMESTAMPTZ NOT NULL DEFAULT now()
索引：(node_id, pve_vmid, created_at DESC)
```

- PVE 操作受理成功后同步写记录；写失败 → 返回 500（操作已受理，前端提示可刷新确认），保证审计完整性优先
- 查询接口 `GET /vms/{id}/operations`：按时间倒序分页；`ext-` 标识同样支持（按 node+vmid 查）
- 操作记录不随 VM 行删除而删除（无外键 ON DELETE）

### D6: 认领接口保留 `/vms/import`，IP 变可选；下线 `/vms/unmanaged`

- 请求体新增可选 `ip` 字段（从区域池分配）；不传则 `ip_id` 保持 NULL（DB 已支持）
- 移除原"强制分配/复用静态 IP"逻辑，保留节点校验与幂等
- `GET /vms/unmanaged` 下线（BREAKING，见 spec REMOVED）；前端认领入口改为基于列表 external 条目
- 认领成功后 source 置为 `claimed`

### D7: 节点故障标记复用现有 warnings 机制

- 节点禁用/查询失败 → 该节点 VM 不显示（现有行为）+ warnings 数组携带节点级故障（节点名、原因、enabled 状态）
- 前端列表页将节点故障 warning 渲染为醒目 banner（区分于常规提示）
- 节点恢复后自动重新出现（无状态缓存，天然满足）

## Risks / Trade-offs

- 列表接口响应体变大（全量 VM）→ 单节点规模小，且 limit 上限 100 仍是最终返回条数；合并仅影响服务端内存
- 操作记录写失败时 PVE 已受理但接口返回错误 → 前端提示"操作已受理但记录失败，请刷新确认"；错误码明确
- `source` 列默认值对存量 Spark 创建行无影响，回填仅针对 `image_id IS NULL` 的行；若历史数据存在"手工创建且无镜像"的行会误标 claimed → 迁移前人工确认，或按创建路径判断（本系统创建必带 image_id，误判概率极低）
- 破坏性契约变更（source 字段、unmanaged 下线、import 请求体）→ 契约双副本同步 + 前端同批改动 + `npm run api:check` 门禁

## Migration Plan

1. 迁移 0008：`vms` 加 `source` 列（默认 `spark_created`）+ 回填 `claimed`；建 `vm_operations` 表与索引
2. 后端：列表合并（D1-D3）、控制放开（D4）、操作记录（D5）、认领改造（D6）分任务落地，各带单测
3. 契约：`docs/openapi.yaml` 与 `api/swagger/openapi.yaml` 双副本同步，`npx --yes @redocly/cli lint` 通过
4. 前端：来源徽章、external 条目操作入口、节点故障 banner、认领表单 IP 可选；移除 unmanaged 弹窗调用；`npm run api:check` 通过
5. e2e：fake PVE 扩展 external 直接控制 + 操作记录断言；`go test -tags=e2e ./e2e/ -count=1 -v` 通过
6. 回滚：后端回滚需连带迁移回退（source 列可保留，`vm_operations` 表保留无害）

## Open Questions

- 操作记录查询是否需要跨 VM 的全局过滤（按节点/动作/时间）？当前仅按 VM 查询可满足审计基本诉求，全局检索可后补
