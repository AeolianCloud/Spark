## Purpose

管理 PVE 集群节点名(PveName)与业务名(Name)的分离:登记节点时自动探测 PVE 集群节点名并持久化,所有节点 API 调用使用集群节点名,避免业务名与集群节点名不一致导致的 595 错误。

## Requirements

### Requirement: 登记节点时自动探测集群节点名

系统 SHALL 在登记节点时调用 PVE `GET /nodes` 探测集群节点名列表,并持久化到节点的 PVE 集群节点名字段(PveName)。探测到的集群节点名有多个时,系统 SHALL 选择与业务名一致的一个;没有一致的名称时,系统 SHALL 拒绝登记并返回明确错误。

#### Scenario: 业务名与集群节点名一致

- **WHEN** 管理员登记节点,host 对应集群包含节点 `aeoliancloud`,业务名填 `aeoliancloud`
- **THEN** 系统探测到集群节点 `aeoliancloud`,持久化 PveName=aeoliancloud,登记成功

#### Scenario: 业务名与集群节点名不一致

- **WHEN** 管理员登记节点,业务名填 `aeolian`,但集群真实节点名为 `aeoliancloud`
- **THEN** 系统探测到集群节点 `aeoliancloud`(与业务名 `aeolian` 不一致),拒绝登记并返回错误,提示正确集群节点名

#### Scenario: 探测失败

- **WHEN** 管理员登记节点,但节点 API 不可达或探测失败
- **THEN** 系统拒绝登记并返回节点不可达错误,不持久化节点

### Requirement: 节点 API 调用使用集群节点名

系统 SHALL 使用 PVE 集群节点名(PveName)构建所有 `/nodes/{node}/...` API 请求:包括 VM 列表与状态查询、VM 生命周期操作、镜像查询与可达性探测。业务名(Name) SHALL NOT 用于 PVE API 路径。

#### Scenario: VM 列表使用集群节点名

- **WHEN** 系统对 PveName=aeoliancloud 的节点查询 VM 列表
- **THEN** 请求路径为 `/nodes/aeoliancloud/qemu`,而非业务名 `aeolian`
