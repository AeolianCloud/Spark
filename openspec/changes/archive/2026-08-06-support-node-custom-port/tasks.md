## 1. 数据库迁移

- [x] 1.1 新增 `database/migration/0005_add_node_port.sql`:`ALTER TABLE pve_nodes ADD COLUMN port INTEGER NOT NULL DEFAULT 8006`
- [x] 1.2 在 0005 迁移中回填存量数据:host 匹配 `:\d+$` 的行,把数字后缀解析为 port 并从 host 剥离(参照 design.md D5 SQL)

## 2. pve 客户端端口支持

- [x] 2.1 `pve/client.go` 新增 `WithPort(port int)` Option:port 为 0 时忽略,否则用 port 覆盖 baseURL 端口(保持 `stripHostPort` 语义不变)
- [x] 2.2 `pve/client_test.go` 新增测试:WithPort 覆盖默认端口、port=0 走默认端口、与 stripHostPort 组合不产生双端口

## 3. 模型与仓库

- [x] 3.1 `model/entities.go`:`PVENode` 新增 `Port int` 字段(json tag `port`,默认 8006 语义由 service 保证)
- [x] 3.2 `repository/node_repo.go`:`nodeCols` 增加 port 列,CreateNode/GetNode/UpdateNode/listNodes 的 SQL 与 Scan 同步更新
- [x] 3.3 仓库相关测试(如有)补 port 读写断言

## 4. service 层端口解析与客户端工厂

- [x] 4.1 `service/zone_service.go` 新增 host 解析工具:去除 scheme 后解析 `:port` 后缀(纯数字 1-65535 合法;多冒号 IPv6/非数字后缀返回 badRequest;无后缀默认 8006),host 与 port 分离
- [x] 4.2 `CreateNode`/`UpdateNode` 调用解析工具:host 存储纯地址,端口写入 `node.Port`
- [x] 4.3 `service/vm_service.go`:`newClient` 工厂签名增加 `port int` 参数,内部以 `pve.WithPort(port)` 构建;所有调用点(vm_service.go、vm_query.go、zone_service.go selectReachableNode)传 `node.Port`
- [x] 4.4 `service/zone_service.go` `selectReachableNode` 探测使用节点端口

## 5. handler 与 API 文档

- [x] 5.1 `api/handlers/zone_handler.go`:`nodeResponse` 增加 `port` 字段并回显;`nodeRequest` 保持无 port 字段(端口在 host 内)
- [x] 5.2 `api/router.go` 节点相关 OpenAPI 注释更新:host 参数注明支持 `host:port` 形式,响应包含 port 字段

## 6. 测试与验证

- [x] 6.1 机械更新 service 层测试中所有内联 `newClient` 工厂签名(约 20 处,port 传 0)
- [x] 6.2 新增/更新 service 测试:host 带端口登记成功且 Port 正确、非法端口拒绝、无端口默认 8006
- [x] 6.3 `e2e/e2e_test.go` 适配新工厂签名,必要时补自定义端口场景
- [x] 6.4 运行 `go build ./...`、`go vet ./...`、`go test ./...` 全量通过
- [x] 6.5 本地验证:登记 `host:8007` 节点 → 节点列表 port=8007 → 获取 VM 状态连接 8007 成功
