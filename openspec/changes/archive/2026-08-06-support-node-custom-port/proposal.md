## Why

添加 PVE 节点时,用户在 host 中以 `host:port` 形式配置了非默认端口(如 `117.177.33.8:8007`),但后端 `pve.NewClient` 的 `stripHostPort` 会把 `:port` 剥离并固定使用默认端口 8006,且 `pve_nodes` 表没有端口字段。导致获取 VM 状态等所有 PVE 调用都打到 `https://117.177.33.8:8006`,报 `dial tcp ...:8006: connect: connection refused`。

## What Changes

- `pve_nodes` 表新增 `port` 列,持久化节点自定义端口(空/0 表示默认 8006)。
- `model.PVENode` 增加 `Port` 字段。
- 节点仓库的 Create/Get/List/Update 全部读写 `port` 列。
- `pve.NewClient` 增加 `WithPort(port)` Option,不再剥离 host 中的端口后就丢弃(host 中的 `:port` 在节点登记时解析并存储)。
- `ZoneService.CreateNode/UpdateNode` 从 host 解析出端口并落库;新增端口校验(合法 1-65535,默认 8006)。
- 所有构建 PVE 客户端的地方(VM 生命周期、查询、可达性探测)改用节点存储的端口。
- API 文档与 OpenAPI 注释同步更新(host 支持 `host:port` 形式)。

## Capabilities

### New Capabilities

- `node-custom-port`:节点自定义端口的登记、持久化与使用——host 携带 `:port` 时解析并存储端口,所有 PVE API 调用与可达性探测均使用该端口连接。

### Modified Capabilities

- `zones`:节点管理需求从"主机地址与 API 凭据"扩展为支持自定义端口;登记/更新节点时从 host 解析端口。

## Impact

- 数据库:`database/migration/0005_add_node_port.sql`(新增 migration)。
- `model/entities.go`:`PVENode` 新增 `Port` 字段。
- `repository/node_repo.go`:SQL 与 Scan 增加 port 列。
- `pve/client.go`:`NewClient` 新增 `WithPort` Option;`stripHostPort` 语义调整为解析端口(返回 host 与端口)。
- `service/zone_service.go`、`service/vm_service.go`、`service/vm_query.go`:客户端工厂签名携带端口。
- `api/handlers/zone_handler.go`、`api/router.go`:请求/响应模型与 OpenAPI 注释。
- 测试:`pve/client_test.go`、`service/*_test.go`、`e2e/e2e_test.go`。
