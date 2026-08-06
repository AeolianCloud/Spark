## Context

问题根源见 proposal.md - Why:`pve.NewClient`(pve/client.go:63)通过 `stripHostPort` 剥离 host 中的 `:port` 后缀,并用 `fmt.Sprintf("https://%s:%d/...", host, defaultPort)` 固定拼接默认端口 8006;同时 `pve_nodes` 表、`model.PVENode`(model/entities.go:13)、`nodeRequest`(api/handlers/zone_handler.go:141)均无端口字段。因此用户以 `117.177.33.8:8007` 形式添加节点后,端口在构造客户端时被丢弃,所有请求打到 8006 报 connection refused。

当前客户端工厂形态:服务层统一通过 `newClient(host, apiUser, apiTokenSecret string) *pve.Client` 构建客户端(vm_service.go:164、zone_service.go:245),测试通过替换该工厂注入假服务器。`WithBaseURL` 已存在但仅用于测试/反代,不适用于逐节点端口。

## Goals / Non-Goals

**Goals:**
- host 携带 `:port` 时,端口被解析、持久化并用于所有 PVE API 调用与可达性探测。
- 存量数据(host 已带 `:port`)通过迁移自动修复,无需人工重新登记。
- 不携带端口的行为与现状完全一致(默认 8006)。

**Non-Goals:**
- 不改 IPv6 支持现状(pve 客户端明确不支持 IPv6 字面量,维持显式报错)。
- 不做端口变更的运行时热更新,端口随节点更新接口变更。
- 不引入独立的 port 请求字段(前端按确认以 `host:port` 形式传参)。

## Decisions

### D1:端口在 service 层解析并落库,而非在 pve 客户端解析

`ZoneService.CreateNode/UpdateNode` 负责从 host 解析 `:port` 后缀,合法端口存入 `pve_nodes.port`;host 字段只保留纯主机地址。客户端构建时使用持久化的 port。

- 备选:让 `pve.NewClient` 保留 host 中的端口(剥离后不再使用默认端口)。
- 弃用理由:端口不落库则节点列表 API、更新接口都无法回显端口,且解析逻辑散落在客户端;落库后所有消费方(查询、探测、生命周期)统一从模型取值,单点可靠。

### D2:`pve.NewClient` 新增 `WithPort(port int)` Option,`stripHostPort` 保留

`WithPort` 覆盖 baseURL 中的端口(port 为 0 时不生效,保持默认 8006)。`stripHostPort` 及其现有语义保留:对库中误存 `host:port` 的脏数据仍剥离端口,避免 `https://host:8006:8006/...` 双端口 base URL;`TestNewClientStripsHostPort`(client_test.go:113)继续通过,保证兼容。

- 备选:修改 `NewClient` 签名增加 port 参数。
- 弃用理由:破坏全部现有调用点与测试;Option 形式与既有 `WithBaseURL/WithTimeout/WithInsecure` 风格一致。

### D3:客户端工厂签名携带端口

`newClient` 工厂签名由 `func(host, apiUser, apiTokenSecret string) *pve.Client` 改为 `func(host string, port int, apiUser, apiTokenSecret string) *pve.Client`,内部用 `pve.WithPort(port)` 构建。所有调用点(vm_service.go:429/620/634/647/673/754、vm_query.go:196/261、zone_service.go:250)传入 `node.Port`。

- 备选:工厂接收整个 `model.PVENode`。更通用,但破坏面相同且弱化测试注入的直观性;单参数扩展足以覆盖端口场景。
- 影响:service 层测试中所有内联 `newClient` 定义需同步加 port 参数(port 传 0 走默认端口,现有假服务器不受影响)。

### D4:端口解析规则与校验集中在 service 层

- 去掉 scheme(`http(s)://` 前缀)后,取最后一个 `:` 后的后缀:纯数字且 1-65535 → 端口;无 `:` → 默认 8006;含多个 `:`(IPv6 字面量)或后缀非数字 → `badRequest`(与 pve 客户端"IPv6 不支持"语义一致,但提前到登记时拒绝,而不是首次请求才报错)。
- 解析结果写入 `node.Port`,host 保存纯地址。
- 复用点:解析逻辑独立为 service 内工具函数,便于单测。

### D5:数据库迁移同时修复存量数据

`database/migration/0005_add_node_port.sql`:

```sql
ALTER TABLE pve_nodes ADD COLUMN port INTEGER NOT NULL DEFAULT 8006;
UPDATE pve_nodes SET port = COALESCE(CAST(substring(host FROM '(\d+)$') AS INTEGER), 8006)
  WHERE host ~ ':\d+$';
UPDATE pve_nodes SET host = regexp_replace(host, ':\d+$', '', 'g') WHERE host ~ ':\d+$';
```

- 新列 `NOT NULL DEFAULT 8006`:存量无端口节点与新建无端口节点统一为 8006,零值歧义消失。
- 两步 UPDATE 把存量 `host:port` 的 host 后缀剥掉并回填 port,彻底修复用户遇到的场景,无需人工干预。
- 若迁移被拆为纯 DDL 审批制,也可只保留 ALTER + 在 service 读取层做归一化兜底,但本仓库 migrate 机制(migrate.go 按序执行)允许数据回填,见 Migration Plan。

### D6:API 响应与 OpenAPI 注释同步

- `nodeResponse` 增加 `port` 字段,节点列表/详情可回显端口(默认 8006 亦返回,消除歧义)。
- router.go 中节点相关 OpenAPI 注释更新:host 描述注明支持 `host:port` 形式。

## Risks / Trade-offs

- [存量节点 host 含端口但迁移失败] → 迁移 SQL 先行在空库/样例库验证;`stripHostPort` 保留作为第二道防线,host 带端口时至少不会产生双端口 URL(最坏仍是 8006 语义,与修改前一致)。
- [port 校验与 pve 客户端规则不一致] → 解析逻辑(数字后缀判定)与 `stripHostPort`(client.go:137)保持同一规则:纯数字才视为端口;IPv6/非数字后缀显式拒绝。
- [测试破坏面] → 所有内联 `newClient` 工厂(约 20 处)需改签名;统一机械替换,port 传 0,假服务器行为不变。
- [API 兼容] → 请求体不变(端口仍在 host 内),响应新增 `port` 字段为 additive 变更,老前端兼容。

## Migration Plan

1. 发布顺序:先上线代码(端口解析在 service 层),后执行/重启触发 migration `0005`(migrate.go 按序自动执行)。
2. 迁移含数据回填,回滚方式:反向 SQL 需保留 host 后缀信息,故不提供自动回滚;若必须回滚,先备份 `pve_nodes` 行(ALTER 前 `CREATE TABLE pve_nodes_backup AS SELECT * FROM pve_nodes`),回滚时恢复备份并删除 `port` 列。
3. 验证:登记 `117.177.33.8:8007` 节点 → 节点列表返回 port=8007 → 查询 VM 状态成功连接 8007。

## Open Questions

无。
