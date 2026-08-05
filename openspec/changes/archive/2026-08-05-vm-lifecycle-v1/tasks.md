## 1. 项目骨架与基础设施

- [x] 1.1 初始化 go module（go+gin），建立目录结构（api/handlers、service、repository、pve、model）
- [x] 1.2 配置管理（yaml/env）：PostgreSQL DSN、密码加密密钥、服务端口
- [x] 1.3 PostgreSQL 接入（连接池、ping 健康检查）与数据库 migration 机制
- [x] 1.4 统一错误响应结构 `{error: {code, message}}` 与 gin 中间件（日志、恢复、请求 ID）
- [x] 1.5 健康检查端点（含 DB 连通性）

## 2. 数据模型

- [x] 2.1 定义全部实体模型：zones、pve_nodes、ip_pools、ip_pool_nodes、ips、storage_types、images、vms
- [x] 2.2 编写 migration：建表、唯一约束（ip 唯一、vm uuid 唯一）、ips.status 索引、外键关系
- [x] 2.3 密码加密/解密工具（应用层对称加密，密钥来自配置）

## 3. PVE 客户端

- [x] 3.1 PVE 客户端基础：API token 认证、HTTPS 调用封装、节点可达性探测（GET /version）
- [x] 3.2 VM 操作封装：创建（POST /nodes/{node}/qemu，一步完成镜像导入/网络/cloud-init）、启动/停止/重启（status 端点）
- [x] 3.3 VM 销毁（DELETE + 结束任务）、配置读取（config）、列表读取（/nodes/{node}/qemu）
- [x] 3.4 镜像导入一步化（create 时 scsi0 内嵌 import-from + ide2 cloudinit 盘）与任务等待（upid 轮询 /nodes/{node}/tasks/{upid}/status）
- [x] 3.5 规格调整封装：CPU/内存（config PUT）、磁盘扩容（resize 端点，只允许增大）

## 4. 区域与节点

- [x] 4.1 区域创建与列表 API（POST/GET /zones）
- [x] 4.2 节点登记与查询（归属区域、host、API 凭据、enabled）
- [x] 4.3 节点可达性检测：按候选顺序取第一个可达节点

## 5. IP 池

- [x] 5.1 IP 池创建（指定区域、网段、网关、DNS），自动展开生成池内 IP 记录
- [x] 5.2 节点勾选可用（ip_pool_nodes 多对多维护）
- [x] 5.3 随机分配实现：free IP 随机选取 + 条件 UPDATE 原子占位（并发安全）
- [x] 5.4 IP 释放（销毁 VM 时标记回 free）与资源不足错误返回

## 6. 镜像目录与存储抽象

- [x] 6.1 存储类型 CRUD（抽象名、display_name、PVE 存储映射）
- [x] 6.2 镜像登记（名称、各节点存在情况、default_user）
- [x] 6.3 按区域镜像查询：汇总各节点取交集，返回 default_user

## 7. VM 生命周期

- [x] 7.1 创建 VM：参数校验（存储类型/镜像/区域/IP 可分配）→ 随机分配 IP → 落库 → 异步触发 PVE 创建链（create 时 scsi0 内嵌 import-from 导入镜像 + ide2 cloudinit 盘 + bootdisk/scsihw + vmbr0 + cloud-init 注入用户名/密码/静态 IP，一次完成）
- [x] 7.2 创建结果返回：立即返回 VM 信息（含 IP、creating 状态）；异步失败标记 provision_error
- [x] 7.3 启动/停止/重启操作 API（POST /vms/:id/start|stop|restart）
- [x] 7.4 销毁 API：触发 PVE 销毁 + 释放 IP（POST /vms/:id/destroy）
- [x] 7.5 规格调整 API：CPU/内存升降配、磁盘只增（校验不得小于现值）

## 8. 穿透式查询

- [x] 8.1 VM 列表：按区域分组节点 → 每节点 1 次 PVE list 调用 → 与本地元数据合并返回
- [x] 8.2 VM 详情：穿透 PVE 实时状态，PVE 不存在时返回 creating/失败状态
- [x] 8.3 节点宕机场景处理：列表部分失败信息、详情明确报错

## 9. 集成验证

- [x] 9.1 单元测试：IP 随机分配与原子占位（并发用例）、磁盘只增校验、镜像交集逻辑
- [x] 9.2 端到端验证：登记区域/节点/IP 池/存储类型/镜像 → 创建 → 生命周期操作 → 销毁回收 IP
- [x] 9.3 补充 README：部署步骤、PVE 节点准备（cloud 镜像放置、vmbr0、API token 创建）
