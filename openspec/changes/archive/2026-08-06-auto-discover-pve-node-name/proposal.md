## Why

节点登记时 `name` 同时承担"业务名"和"PVE 集群节点名"两个职责,代码用 `node.Name` 拼接 `/nodes/{name}/...` 请求 PVE。用户登记业务名 `aeolian`(集群真实节点名是 `aeoliancloud`)后,所有节点 API 调用返回 `595 Connection refused`(空 body),VM 列表、生命周期操作全部失败,且错误信息难以定位。

## What Changes

- `PVENode` 新增 `PveName` 字段(PVE 集群节点名),与业务 `Name` 分离。
- 登记节点时自动探测:调用 `GET /nodes` 获取集群节点名列表,校验并存储 `PveName`。
- 所有 `/nodes/{node}/...` 请求改用 `PveName`,不再使用业务 `Name`。
- 数据库迁移回填存量数据:`PveName` 缺省等于 `Name`(存量行为不变,用户可手动修正)。
- 节点更新接口支持修改 `PveName`;API 响应回显 `pve_name`。
- 错误诊断改进:节点请求失败时,日志/错误信息携带使用的 PVE 节点名。

## Capabilities

### New Capabilities

- `pve-node-name`:PVE 集群节点名管理——登记节点时自动探测并存储集群节点名,所有节点 API 调用使用该名称。

### Modified Capabilities

- `zones`:节点管理需求扩展为业务名与 PVE 集群节点名分离,登记时自动探测。
- `node-custom-port`(上一 change 归档的能力):节点 API 调用规则中"节点标识"从业务名改为 PVE 集群节点名。

## Impact

- 数据库:`database/migration/0006_add_pve_name.sql`。
- `model/entities.go`:`PVENode` 新增 `PveName`。
- `repository/node_repo.go`:增删改查同步 `pve_name` 列。
- `pve/client.go`:`ListNodes`(GET /nodes)客户端方法。
- `service/zone_service.go`:CreateNode 探测集群节点名;UpdateNode 支持修改 PveName。
- `service/vm_service.go`、`service/vm_query.go`:所有 `client.ListVMs(ctx, node.Name)` 等调用改用 `node.PveName`。
- `api/handlers/zone_handler.go`、OpenAPI:`pve_name` 字段。
- 测试:service 层、e2e 同步。
