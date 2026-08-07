# Spark 领域上下文

Spark 是一个对接 Proxmox VE（PVE）的公有云后端：管理可用区、节点、镜像、存储与 IP 池，并通过 PVE 标准 API 完成虚拟机的全生命周期管理。本词汇表是领域术语的唯一权威来源，代码与文档中的命名必须与此一致。

## 资源分组

**Zone（可用区）**:
部署区域，用于将节点、IP 池和虚拟机分组。
_Avoid_: 数据中心、region、区域（指 Zone 时）

## 基础设施

**PVE**:
Proxmox VE，底层虚拟化平台。系统通过其标准 HTTP API 执行虚拟机与节点操作。
_Avoid_: Proxmox（仅指具体产品名时可保留）

**Node（节点）**:
已注册的 PVE 物理节点记录，携带 API 访问凭据（API 用户与令牌）。业务名 `Name` 与 PVE 集群节点名 `PveName` 分离，`PveName` 为空时沿用业务名；`PVENode.Host` 字段指 PVE API 主机地址，不受 Avoid 词限制。
_Avoid_: host、物理机、server（除 `PVENode.Host` 字段语义外）

## 网络

**IPPool（IP 池）**:
Zone 内的一个 CIDR 地址池，绑定网关与 DNS，可通过 IPPoolNode 白名单决定哪些节点可从此池分配地址。
_Avoid_: 网段、地址段

**IP（地址）**:
IP 池中的单个地址，状态为 free/used；被分配时绑定到 VM。
_Avoid_: 公网 IP、弹性 IP

## 镜像与存储

**Image（镜像）**:
已注册的云镜像，携带默认登录用户与下载地址（download_url）；镜像在各节点上的存在情况不落库，以 PVE 为权威源实时扫描判定。
_Avoid_: 模板、template

**StorageType（存储类型）**:
对 PVE 存储（如 local-ssd）的显示名抽象，供 VM 选择磁盘目标。
_Avoid_: storage、磁盘类型（指抽象记录时）

## 计算实例

**VM（虚拟机）**:
由业务记录（含 pve_vmid 与预配置状态）和 PVE 侧真实虚拟机组成的整体。实时运行状态不落库，一律向 PVE 透传查询。
_Avoid_: instance、guest、虚机（代码与文档统一用 VM）
