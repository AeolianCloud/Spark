## Why

项目需要一个面向公有云场景的 PVE 虚拟化管理后端（go+gin），对前端只暴露自有 REST API。当前仓库尚无任何代码，第一版需要从零搭建 VM 生命周期管理的基础能力，同时建立区域（zone）隔离、IP 池分配、镜像与存储抽象等支撑设施，为后续多区域集群扩展打地基。

## What Changes

- 新建 go+gin 后端服务，仅暴露自有 REST API，不直接透传 PVE API
- 引入 PostgreSQL 存储领域元数据（VM 元数据、区域/节点、IP 池、存储类型、镜像目录）
- 新增 VM 生命周期能力：创建（cloud 镜像 import）、启动、停止、重启、销毁、调整规格
- VM 状态采用穿透式查询：DB 不存状态镜像，查询时实时调用 PVE API
- 新增区域（zone）模型：一个区域包含多个 PVE 节点，跨区域默认不互通
- 新增 IP 池：按区域划分、按节点勾选可用；创建 VM 时由后端随机分配 IP（前端不可见），强制全部分配
- 新增存储抽象：抽象存储类型（如 ssd/普通磁盘）映射到 PVE 实际存储（如 local-ssd/local-hdd），可配置
- 新增镜像目录：不同节点镜像同名，镜像查询汇总各节点取交集
- cloud-init 注入：用户名按镜像默认（如 debian→debian）、密码、静态 IP；网络固定 vmbr0
- 规格调整规则：CPU/内存可升降配，磁盘只能扩容

## Capabilities

### New Capabilities

- `vm-lifecycle`: VM 的创建/启动/停止/重启/销毁/调整规格，穿透式状态查询，异步创建流程
- `zones`: 区域与 PVE 节点的管理，区域隔离语义（跨区不互通），节点可用性
- `ip-pool`: 按区域划分的 IP 池，节点勾选可用白名单，随机分配与释放
- `image-catalog`: cloud 镜像目录，按节点查询取交集，镜像默认用户名
- `storage-types`: 抽象存储类型定义及其到 PVE 实际存储的映射

### Modified Capabilities

（无，首次变更）

## Impact

- 新仓库：spark 后端服务，技术栈 go + gin + PostgreSQL + PVE API
- 新增基础设施：项目骨架、数据库 schema、PVE 客户端
- 对外 API 契约：VM/区域/IP 池/镜像/存储类型的 REST 端点
- 依赖：PVE 节点 API（QEMU 虚拟机接口）、cloud 镜像（qcow2 import）
