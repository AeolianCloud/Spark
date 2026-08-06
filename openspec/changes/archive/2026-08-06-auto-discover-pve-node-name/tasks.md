## 1. 数据库迁移

- [x] 1.1 新增 `database/migration/0006_add_pve_name.sql`:加列 `pve_name TEXT NOT NULL DEFAULT ''`,回填 `UPDATE pve_nodes SET pve_name = name`(注释说明分离意图)
- [x] 1.2 更新 `database/migrate_test.go` 期望迁移列表追加 `0006_add_pve_name.sql`

## 2. pve 客户端

- [x] 2.1 `pve/client.go` 新增 `ListNodes(ctx) ([]NodeInfo, error)`(`GET /nodes`,NodeInfo 含 `node` 字段,注释中文)
- [x] 2.2 `pve/client_test.go` 补 ListNodes 测试(成功/上游错误)

## 3. 模型与仓库

- [x] 3.1 `model/entities.go`:`PVENode` 新增 `PveName string`(json tag `pve_name`)
- [x] 3.2 `repository/node_repo.go`:`nodeCols` 加 `pve_name`,Create/Get/Update/List 的 SQL 与 Scan 同步
- [x] 3.3 检查并适配其他引用 `nodeCols` 的仓库(ip_pool_repo.go 等)Scan
- [x] 3.4 `repository/node_repo_test.go` 补 PveName 读写断言
- [x] 3.5 `repository/ip_pool_repo_test.go` 补 GetPoolNodes mock 用例(覆盖 pve_name 列,防列错位回归)

## 4. service 层

- [x] 4.1 `service/zone_service.go` CreateNode:探测集群节点名(GetNodes),业务名匹配逻辑;无匹配返回错误并列出集群真实节点名;探测失败拒绝登记
- [x] 4.2 `service/zone_service.go` UpdateNode:host/name 变化时重新探测;支持显式 `pve_name` 覆盖(按 D2)
- [x] 4.3 `service/vm_query.go`、`service/vm_service.go`:所有 `node.Name` → `node.PveName`(空值兜底 Name),含可达性探测路径;镜像路径同步:`image_repo.go` `EnabledNodeNamesByZone` 改查 `pve_name`(空值兜底)、`vm_service.go` `image.NodeImages` 键与 `CreateVM` 节点名用 PveName、`image_service.go` 交集校验同步
- [x] 4.4 节点调用失败日志追加 `pve_node` 字段

## 5. handler 与 API 文档

- [x] 5.1 `api/handlers/zone_handler.go`:`nodeResponse` 回显 `pve_name`;UpdateNode 请求支持 `pve_name`
- [x] 5.2 OpenAPI(`api/swagger/openapi.yaml` + `docs/openapi.yaml` 同步):NodeResponse/NodeRequest 的 `pve_name` 字段与描述

## 6. 测试与验证

- [x] 6.1 service 层测试:mock PVE 补 `/nodes` 端点;断言"业务名 != 集群名"时拒绝登记且提示真实名
- [x] 6.2 vm 测试:节点数据 PveName 与路径断言一致(如 PveName=aeoliancloud 时请求 `/nodes/aeoliancloud/qemu`)
- [x] 6.3 `e2e/e2e_test.go`:fake PVE 增加 `/nodes` 端点;补"业务名与集群名不一致被拒"场景
- [x] 6.4 运行 `go build ./...`、`go vet ./...`、`go test ./...` 全量通过
- [x] 6.5 本地验证:登记 `aeolian`(集群名 aeoliancloud)被拒并提示;登记 `aeoliancloud` 成功;`GET /vms` 200
