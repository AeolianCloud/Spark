## Context

现有 VM 列表/详情为透传模型（设计 D1）：本地 `vms` 表只存元数据（含 `pve_vmid`），实时状态每次从 PVE 节点查询。`mergeVMListItems` 只合并本地有记录的行，PVE 上无本地记录的 VM 被刻意跳过。`vms` 表约束（image_id/storage_type_id/password_encrypted NOT NULL + FK）与 IP 领取流程（migration 0002 的 INSERT vms → 领 IP → 回填 ip_id 事务约定）均已成型，导入需要兼容它们。详见 proposal.md - Why。

## Goals / Non-Goals

**Goals:**
- 将 PVE 已有 VM 纳管为托管 VM，复用现有全部生命周期与透传查询能力
- 导入 IP 优先复用 PVE 静态 IP，无静态 IP/无匹配池时回退池分配
- 防止重复导入；导入失败不产生脏记录

**Non-Goals:**
- 不做"展示未托管 VM"的只读模式（仅导入入口内的候选选择，不进入 /vms 列表）
- 不修改导入 VM 的 PVE 配置（网络、磁盘、cloud-init 一律不动）
- 不提供批量导入、模板/镜像识别、计费关联

## Decisions

### D1：API 形态

- `GET /vms/unmanaged?node_id=X`：未托管候选列表。用查询参数而非嵌套路径（`/zones/{z}/nodes/{n}/qemu`），与项目平铺风格（/vms、/zones、/nodes）一致；候选 VM 响应体为轻量对象（vmid/name/status/cpu/mem_mb/disk_gb），磁盘大小由服务层遍历 PVE config 磁盘键解析求和（`scsi\d+|ide\d+|sata\d+|virtio\d+|efidisk\d+|tpmstate\d+` 的 size 字段，复用 `parseDiskSizeGB`）。
- `POST /vms/import`，请求体 `{zone_id, node_id, pve_vmid, ip_pool_id?, name?}`：`ip_pool_id` 可选（缺省自动选池），`name` 可选（缺省取 PVE 配置名）。返回 201 + Location + 完整 VM 负载（透传状态）。
- 替代方案（POST /nodes/{id}/qemu 直接创建式导入）被否：会让 /nodes 承担创建语义，且与现有 /vms 集合语义冲突。

### D2：幂等与唯一约束

- 服务层先查 `GetVMByNodeVMID` 拒绝重复导入（409 vm_already_managed），数据库用**部分唯一索引** `UNIQUE (node_id, pve_vmid) WHERE pve_vmid > 0` 兜底——供给中的 VM 多行 `pve_vmid=0` 必须排除，否则全表唯一互相冲突。并发下 23505 映射为同一 409。

### D3：IP 策略（优先复用静态 IP）

1. 从 PVE config 读 `ipconfig0`，解析 `ip=<addr>/<prefix>`（逗号分隔键值）。
2. 有静态 IP：在 zone 的池中找 `network_cidr` 包含该地址、`ip_pool_nodes` 白名单含该节点、且该地址在池中状态为 free 的池 → 事务内**按地址精确领取**（新增 `ClaimIPByAddressTx`：`UPDATE ips SET status='used', vm_id=$x WHERE pool_id=$p AND ip=$addr AND status='free'`，影响行数为 0 视为被并发占用 → 回退池分配）。
3. 无静态 IP 或无匹配池 → 回退现有 `selectPoolAndNode` 同款逻辑（用户未指定池时）或指定池的 `ClaimFreeIP`。
4. 池耗尽 → 复用 `ip_exhausted`（409）。

### D4：可空字段与 Go 类型

- `model.VM.ImageID`/`StorageTypeID` 改 `*int64`（与现有 `IPID *int64` 风格一致，避免 0 魔法值）；`PasswordEncrypted` 保持 string，读取用 `COALESCE(password_encrypted,'')`。
- migration 0007 放宽三个 NOT NULL（FK 保留）；`vmCols` 增补 COALESCE；handler `vmResponse` 的 `image_id`/`storage_type_id` 加 `omitempty`（契约同步为可空）。
- 导入的 VM 走与创建相同的 IP 领取事务约定：单事务 INSERT（ip_id NULL）→ 领 IP → 回填 ip_id。

### D5：导入后的生命周期

- 导入即托管：`pve_vmid` 非零、`provision_error` 为空 → 现有 `vmAndNode`/`mergeVMListItems` 路径直接生效（start/stop/restart/resize/destroy/详情透传全部可用，零额外改动）。
- destroy 语义对导入 VM 同样适用：PVE 删除（purge）+ 释放 IP + 删行。

## Risks / Trade-offs

- [静态 IP 复用时地址已被并发占用] → 按地址领取的 UPDATE 原子失败 → 回退从池分配新 IP，请求不失败（记录新 IP）。
- [导入的 VM 本地 IP 与 PVE 网卡实际配置可能不一致（回退分配场景）] → 导入响应与详情展示分配结果；PVE 配置不被修改是刻意决策（避免动用户 VM），风险记录在案。
- [磁盘大小解析不完整（未识别特殊盘键）] → 缺失的键按 0 处理，disk_gb 可能偏小；`resize` 只允许增大，不影响正确性。
- [image_id/storage_type_id 可空影响既有创建/响应契约] → 契约显式声明可空，前端类型同步；创建路径仍必填（CreateVMRequest 不变）。

## Migration Plan

- migration 0007：`ALTER TABLE vms ALTER COLUMN image_id/storage_type_id/password_encrypted DROP NOT NULL` + `CREATE UNIQUE INDEX vms_node_vmid_key ON vms(node_id, pve_vmid) WHERE pve_vmid > 0`。
- 现有数据不受影响（NOT NULL 放宽不校验存量）；回滚只需撤销 0007（重建约束），唯一索引删除即可。
- 前端与后端同步发布：契约字段可空是向后兼容变更（客户端忽略未知字段原则）。

## Open Questions

无。
