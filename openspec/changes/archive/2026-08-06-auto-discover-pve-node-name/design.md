## Context

问题见 proposal.md - Why:`PVENode.Name` 同时充当业务名与 PVE 集群节点名,代码用 `node.Name` 拼接 `/nodes/{name}/qemu`(vm_query.go:197/262、vm_service.go 多处)。PVE 集群 API 对 `/nodes/{node}` 中的节点名做集群成员匹配,不存在的名字返回 595 "Connection refused"(空 body),错误难以定位。

当前形态:节点登记(zone_service.go CreateNode)只校验 host/api_user/api_token 并落库,不做任何 PVE 侧探测;`pve.Client` 已有 `ProbeVersion`/`Ping`,但无 `GET /nodes` 客户端方法;`nodeResponse` 已回显 `name/host/port`。

## Goals / Non-Goals

**Goals:**
- PVE 集群节点名与业务名分离,登记时自动探测,杜绝 595。
- 存量节点(未探测过)行为不回归:`PveName` 缺省 = `Name`。
- 探测失败时登记失败并给出可操作的错误(提示集群真实节点名)。

**Non-Goals:**
- 不做集群多节点的运行时同步(探测仅在登记/更新节点时进行)。
- 不引入 PVE 侧节点名的模糊匹配/自动纠错——只做精确匹配 + 明确报错。
- 不改 VM 生命周期与查询逻辑,只替换路径中的节点标识来源。

## Decisions

### D1:PveName 作为独立字段落库,登记时探测

`PVENode` 新增 `PveName string`,迁移 `0006_add_pve_name.sql` 加列 `pve_name TEXT NOT NULL DEFAULT ''`,存量回填 `UPDATE pve_nodes SET pve_name = name`。CreateNode 流程变为:校验业务名/host/凭据 → 探测集群节点名 → 匹配/存库 → 落库。

- 备选 A:删除业务名,直接用集群节点名当 name。弃用:业务名是前端展示与去重键,且无法处理"业务名 != 集群名"的既有诉求。
- 备选 B:不落库、每次请求实时探测。弃用:每次调用多一次 PVE 往返,且探测失败时生命周期操作也无法进行。

### D2:探测返回多节点时,优先匹配业务名,否则报错

`GET /nodes` 返回集群全部节点名。CreateNode 探测后:
- 集群节点名 == 业务名 → PveName = 业务名,成功;
- 多个节点且唯一匹配业务名 → 同上;
- 无匹配 → 返回 KindNodeUnavailable 变体错误,错误消息列出集群真实节点名(提示用户)。
- 探测本身失败(不可达/TLS/401)→ 拒绝登记,复用现有 nodeUnavailablef 语义。

更新节点(UpdateNode)时:仅当 host 或 name 变化时重新探测;PveName 随 name 一起更新。为避免过度复杂,UpdateNode 规则:若请求体包含显式 `pve_name`,直接采用(跳过探测匹配);否则走探测匹配。

- 备选:API 增加显式 `pve_name` 请求字段为主路径,探测为辅助。考虑:前端尚无该字段,探测是默认 UX;显式字段作为逃生通道保留在 UpdateNode。

### D3:所有 PVE 路径改用 PveName

`vm_query.go`、`vm_service.go` 中全部 `client.ListVMs(ctx, node.Name)` 等调用改为 `node.PveName`(空值兜底回退 `node.Name`,兼容探测前存量数据)。`pve.Client` 新增 `ListNodes(ctx) ([]NodeInfo, error)`(`GET /nodes`,仅取 `node` 字段)。

- 兜底原则:`PveName == ""` 时用 `Name`,保证迁移回填前的脏数据不炸;迁移回填后理论不会出现空值。

### D4:错误诊断改进

- `UpstreamError` 保持透传(595 空 body 会显示 "status 595: empty response"),service 层在节点调用失败日志中追加 `pve_node` 字段(使用的集群节点名),便于对照集群真实名。
- CreateNode 探测失败的错误消息包含探测到的集群节点名列表(无凭据信息)。

## Risks / Trade-offs

- [探测依赖节点可达] → 与既有"登记即可达"心智一致;失败时明确报错,不静默落库。
- [存量数据 PveName 回填 = Name 可能错误] → 兜底规则保证行为不回归,用户可通过 UpdateNode 显式修正。
- [多节点集群下探测延迟] → 单次 GET /nodes,受现有 nodeProbeTimeout 约束。
- [测试破坏面] → service 层测试中节点 mock 需补 /nodes 端点;vm_query/vm_service 测试的假服务器路径断言由 `Name` 改 `PveName`(mock 数据里 PveName=Name 时断言不变)。

## Migration Plan

1. 代码先上线(PveName 空值兜底 Name),迁移 `0006` 自动执行回填。
2. 回滚:迁移为 additive(新列 DEFAULT ''),无需数据迁移,回滚删列即可。
3. 验证:登记 `aeolian`(集群名 aeoliancloud)→ 被拒并提示真实名;登记 `aeoliancloud` → 成功;`GET /vms` 返回 200。

## Open Questions

无。
