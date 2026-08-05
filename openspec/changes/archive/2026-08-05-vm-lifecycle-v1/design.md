## Context

全新仓库，无既有代码。目标见 proposal.md 的 Why。技术约束：go + gin、PostgreSQL（连接由用户提供）、对接 PVE 标准 API、状态穿透式（DB 不存状态镜像）。业务约束：只暴露自有 REST API、无计费、强制分配 IP、cloud 镜像 import 方式创建。

## Goals / Non-Goals

**Goals:**

- 建立可运行的后端骨架：HTTP 服务、PostgreSQL 接入、PVE 客户端
- 覆盖第一版全部能力：VM 生命周期、区域、IP 池、镜像目录、存储抽象
- 创建流程在线性时间内完成 IP 分配与元数据落库，PVE 侧操作为异步

**Non-Goals:**

- 不做计费、配额、认证鉴权（认证留待前端接入时定，见 Open Questions）
- 不做任务持久化队列与回调（第一版前端轮询状态即可）
- 不做多区域镜像同步/拷贝（约定各节点镜像同名，查询取交集）
- 不做磁盘缩容、迁移、克隆、快照等扩展生命周期操作

## Decisions

### D1: 穿透式状态，DB 只存元数据

VM 的实时状态每次查询时直接调 PVE 节点 `/nodes/{node}/qemu`（节点级 list 接口，一次返回该节点全部 VM），与本地元数据合并后返回。DB 中不维护 status 字段，避免双源不一致问题。

- 备选：本地状态镜像 + 定时同步 → 有失准风险，且查询走 PVE 成本可控（500 客户量级），穿透式更简单可靠。
- 成本：每次列表查询 = 涉及节点数 × 1 次 PVE 调用，量级可接受。

### D2: 数据库 schema

```
zones            id, name, created_at
pve_nodes        id, zone_id, name, host, api_user, api_token_secret, enabled, created_at
ip_pools         id, zone_id, name, network_cidr, gateway, dns, created_at
ip_pool_nodes    ip_pool_id, node_id            -- 勾选可用白名单 (多对多)
ips              id, pool_id, ip, status(free/used), vm_id, updated_at
storage_types    id, name(如 ssd), display_name, pve_storage(如 local-ssd), created_at
images           id, name(如 debian-12-cloud), default_user, node_images JSONB{节点→存储路径/存在标志}, created_at
vms              id, uuid, name, zone_id, node_id, pve_vmid, image_id, storage_type_id,
                 cpu, mem_mb, disk_gb, ip_id, password_encrypted, created_at, updated_at
```

- `ips.status` 加索引，分配用单条条件 UPDATE 保证原子（见 D3）。
- `password_encrypted` 第一版可用应用层对称加密存储（密钥来自配置），避免明文。

### D3: IP 分配：随机 + 原子占位

创建 VM 时：

1. 由 zone 选出该区域所有 IP 池 → 池内 `status=free` 的 IP 集合，随机取一个
2. 原子占位：`UPDATE ips SET status='used', vm_id=? WHERE id=? AND status='free'`，影响行数=0 则重选（限次），全部失败返回资源不足
3. IP 关联写入 vms 记录；销毁 VM 时释放回 free

前端不可见具体 IP、不可指定，因此无需"预留/释放"交互。并发安全完全由条件 UPDATE 保证。

### D4: 部署节点选择

创建 VM 的候选节点 = `区域节点 ∩ 该 IP 池勾选可用节点`，按 IP 池勾选顺序取**第一个可达**的节点（可达性 = PVE API 探测成功）。候选为空或全部不可达则拒绝创建。

### D5: 异步创建模型

```
POST /vms  →  校验参数/镜像存在/存储类型/IP 可分配
           →  随机分配 IP (原子)
           →  落库 vms 记录 (state 隐含: PVE 尚无该 VM)
           →  触发 goroutine 异步执行 PVE 创建链
           →  立即返回 201 + VM 信息 (含 IP)
GET /vms/:id → 穿透 PVE: 存在→实时状态; 不存在→创建中/失败
```

PVE 创建链（按序）：

1. `POST /nodes/{node}/qemu` 一步创建：`scsi0="<storage>:0,import-from=<source>"`（PVE 7.0+ 的镜像导入写法）随 create 完成镜像导入，`ide2="<storage>:cloudinit"` 创建 cloud-init 数据盘（未显式传入则 ciuser/cipassword/ipconfig0/nameserver 不生效），同时写入 `bootdisk`/`scsihw`、`net0` 挂 vmbr0，并在同一次 create 中注入 cloud-init 配置（`ciuser`/`cipassword`/`ipconfig0`/`nameserver`/`searchdomain`）
2. 轮询 `qmcreate` 任务：`GET /nodes/{node}/tasks/{upid}/status` 等待 UPID 完成（镜像导入耗时在异步任务内消化）

> 说明：PVE 7/8/9 的 REST API 均无 `/importdisk` 端点（importdisk 只是 qm CLI 命令），镜像导入统一走 create 时的 import-from 磁盘参数；cloud-init 数据盘同样只能在 create 时显式传 `ide2=<storage>:cloudinit` 创建。创建、导入、cloudinit 盘、网络、cloud-init 一次完成。

异步任务失败：第一版仅记录错误日志 + 更新元数据标记（如 `provision_error`），不做自动重试/回滚（见 Risks）。

### D6: 对外 API 形态

```
POST   /zones                    创建区域
GET    /zones                    区域列表
POST   /vms                      {name, cpu, mem_mb, disk_gb, image_id, storage_type_id, zone_id, password}
GET    /vms                      列表 (穿透合并)
GET    /vms/:id                  详情 (穿透)
POST   /vms/:id/start|stop|restart
POST   /vms/:id/resize            {cpu?, mem_mb?, disk_gb?} 磁盘只增, 校验不得小于现值
POST   /vms/:id/destroy          销毁 + 释放 IP
GET    /images?zone_id=         区域镜像交集 + default_user
POST   /storage-types            存储类型 CRUD (配置用)
```

gin 路由 + 标准 REST 语义；错误统一返回 `{error: {code, message}}` 结构。

### D7: PVE 客户端

内聚的 pve 客户端包：基于节点配置（api_user + api_token）访问 `https://{host}:8006/api2/json/...`，封装 qemu 生命周期、config 查询、镜像导入（create 时 scsi0 内嵌 import-from）、任务等待（`upid` 轮询 `/nodes/{node}/tasks/{upid}/status`）。所有 PVE 调用均以"节点"为单位发出，天然支持多节点。

## Risks / Trade-offs

- [异步创建无重试/回滚：import 失败会残留 PVE 侧半成品 VM] → 第一版记录 provision_error 并在详情中暴露；后续版本可加任务表与重试。IP 在失败时暂不释放，避免复用脏 IP，由运营手工回收。
- [穿透查询依赖 PVE 节点在线：节点宕机时该区域 VM 列表不可用] → 单节点故障时列表返回部分失败信息，详情请求明确报错。
- [密码明文传输/存储] → 传输走 HTTPS；存储用应用层加密；密码本身是 cloud-init 一次性注入，安全面有限。
- [镜像交集校验为静态配置：节点上镜像实际缺失不被实时发现] → 第一版以登记为准；PVE 调用失败时创建报错即暴露问题。
- [随机分配可能让同一 IP 池热点不均] → 数据量小可忽略；必要时改轮询。

## Migration Plan

全新项目，无存量数据迁移。部署步骤：建库建表（migration 脚本）→ 启动服务 → 通过管理接口登记区域/节点/IP 池/存储类型/镜像 → 前端接入。回滚：服务整体下线，无持久化副作用（IP 分配记录可手工清理）。

## Open Questions

- 对外 API 的认证方式（token / 无认证）：待前端接入时确定，不影响本变更的领域能力设计。
- 密码是否允许为空（纯密钥注入场景）：默认要求必填，后续按需放开。
