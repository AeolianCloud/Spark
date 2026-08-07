## MODIFIED Requirements

### Requirement: 穿透式状态查询

系统 SHALL 支持查询虚拟机列表与详情，状态数据 SHALL 实时取自 PVE 节点，数据库不维护状态镜像。列表查询 SHALL 合并元数据与实时状态返回。详情查询 SHALL 同时支持本地托管的虚拟机（数字 id）与未纳管虚拟机（external 合成标识 ext-{nodeID}-{vmid}）；未纳管虚拟机详情返回实时状态与 PVE 摘要规格，本地元数据字段（uuid、创建/更新时间、IP）为空。

#### Scenario: 查询虚拟机列表

- **WHEN** 用户请求虚拟机列表
- **THEN** 系统返回包含每个虚拟机元数据与实时状态的列表

#### Scenario: 查询单个虚拟机详情

- **WHEN** 用户请求某个虚拟机详情
- **THEN** 系统返回该虚拟机元数据与实时状态，包含其分配到的 IP

#### Scenario: 查询未纳管虚拟机详情

- **WHEN** 用户以 external 合成标识请求未纳管虚拟机详情
- **THEN** 系统从 PVE 实时读取状态并返回详情（含实时指标），本地元数据字段为空

#### Scenario: 未纳管虚拟机已从 PVE 移除

- **WHEN** 请求详情的 external 标识对应虚拟机已不存在于 PVE
- **THEN** 系统返回资源不存在错误（vm_not_found_on_node）

#### Scenario: 节点不可达时的详情查询

- **WHEN** 请求详情时虚拟机所在 PVE 节点不可达
- **THEN** 系统返回节点不可用错误，不伪造状态
