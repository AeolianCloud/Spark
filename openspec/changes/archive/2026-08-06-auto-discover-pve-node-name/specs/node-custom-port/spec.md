## MODIFIED Requirements

### Requirement: 节点 API 调用使用登记端口

系统 SHALL 使用节点登记的端口构建所有 PVE API 客户端连接：包括 VM 生命周期操作、VM 状态与列表查询、镜像查询以及节点可达性探测。未登记端口时回退到默认端口 8006。所有 `/nodes/{node}/...` 请求中的节点标识 SHALL 使用 PVE 集群节点名（PveName），而非业务名。

#### Scenario: 自定义端口节点的状态查询

- **WHEN** 系统对端口为 8007 的节点查询 VM 状态
- **THEN** 系统连接 `https://<host>:8007/api2/json` 发起查询，而非默认 8006 端口

#### Scenario: 自定义端口节点的可达性探测

- **WHEN** 系统对端口为 8007 的节点执行可达性探测
- **THEN** 探测请求发送到该节点 8007 端口

#### Scenario: 默认端口节点的兼容行为

- **WHEN** 系统连接未登记自定义端口的既有节点（host 无端口后缀）
- **THEN** 行为与修改前一致，连接默认端口 8006

#### Scenario: 节点标识使用集群节点名

- **WHEN** 系统对 PveName=aeoliancloud、业务名 Name=aeolian 的节点发起 VM 列表查询
- **THEN** 请求路径为 `/nodes/aeoliancloud/qemu`
