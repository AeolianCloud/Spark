## MODIFIED Requirements

### Requirement: 认领虚拟机

系统 SHALL 支持将 PVE 节点上已有的虚拟机认领为本地管理的虚拟机。认领请求 MUST 携带区域、节点与 PVE 侧 VMID；系统 MUST 校验区域与节点存在、节点属于该区域且启用、PVE 侧存在该 VMID 的虚拟机。认领成功后，该虚拟机出现在虚拟机列表与详情中，来源标识为 `claimed`。

#### Scenario: 成功认领已有虚拟机

- **WHEN** 用户提交包含区域、节点与 PVE VMID 的认领请求，且该虚拟机尚未被认领
- **THEN** 系统读取 PVE 侧虚拟机配置（名称、规格、磁盘大小），建立本地记录并返回虚拟机信息（含实时状态与来源标识）

#### Scenario: 认领已托管的虚拟机

- **WHEN** 用户尝试认领的 PVE VMID 在该节点上已有本地记录
- **THEN** 系统拒绝认领并返回冲突错误

#### Scenario: 认领不存在的虚拟机

- **WHEN** 用户提交的 PVE VMID 在节点上不存在
- **THEN** 系统拒绝认领并返回资源不存在的错误

#### Scenario: 认领时节点不可达

- **WHEN** 认领请求对应的节点不可达或查询失败
- **THEN** 系统拒绝认领并返回节点不可用的错误，不建立任何本地记录

### Requirement: 认领虚拟机的 IP 策略

认领虚拟机时，分配的 IP SHALL 为可选字段：请求方可在认领时从区域 IP 池分配一个地址记录到本地；未提供 IP 时，系统 MUST 不分配 IP、不要求 PVE 配置中存在静态 IP。已记录 IP 的虚拟机销毁时 SHALL 将 IP 释放回 IP 池。

#### Scenario: 认领时指定 IP

- **WHEN** 认领请求携带 IP 池可用地址
- **THEN** 系统占用该地址并记录到虚拟机本地元数据

#### Scenario: 认领时不指定 IP

- **WHEN** 认领请求未携带 IP 字段
- **THEN** 系统建立本地记录且不关联任何 IP，虚拟机的网络由 PVE 侧配置决定

#### Scenario: 销毁认领虚拟机释放 IP

- **WHEN** 用户销毁已认领且记录了 IP 的虚拟机
- **THEN** 系统销毁 PVE 侧虚拟机并将其记录的 IP 释放回 IP 池

## REMOVED Requirements

### Requirement: 未托管虚拟机候选查询

**Reason**: 虚拟机列表已并入节点上的全部虚拟机（含未认领的外部虚拟机），不再需要单独按节点查询未托管候选的接口；认领入口直接基于列表中的 `external` 虚拟机。
**Migration**: 前端认领入口改为基于列表中的 `external` 来源标识触发；`GET /vms/unmanaged` 接口随本次变更下线。
