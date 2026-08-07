## 1. 数据库

- [ ] 1.1 新增 migration `0007_import_vm.sql`：`vms.image_id`/`storage_type_id`/`password_encrypted` DROP NOT NULL；新增部分唯一索引 `vms_node_vmid_key ON vms(node_id, pve_vmid) WHERE pve_vmid > 0`
- [ ] 1.2 `database/migrate_test.go` 补充 0007 迁移冒烟断言（可空列插入 NULL + 重复 (node_id, pve_vmid) 冲突）

## 2. model 与 repository

- [ ] 2.1 `model.VM.ImageID`/`StorageTypeID` 改为 `*int64`；`vmCols` 及 GetVM/ListVMs/ListVMsPage 扫描兼容 NULL（image_id/storage_type_id 用 *int64，password_encrypted 用 COALESCE）
- [ ] 2.2 `vm_repo.go` 新增 `GetVMByNodeVMID(ctx, nodeID, vmid)`（幂等检查）
- [ ] 2.3 `vm_repo.go` 新增 `ImportVMTx(ctx, tx, vm)`（INSERT 非零 pve_vmid、ip_id NULL、image_id/storage_type_id/password NULL；23505 映射冲突）
- [ ] 2.4 `ip_pool_repo.go` 新增 `ClaimIPByAddressTx(ctx, tx, poolID, ip, vmID)`（按地址原子领取，0 行受影响返回 ErrAllocationRetry 语义）
- [ ] 2.5 仓库层测试：vm_repo_test / ip_pool_repo_pg_test 覆盖新方法

## 3. service

- [ ] 3.1 新增 `service/vm_import.go`：`ImportVM(ctx, req)`（校验 zone/node/pve_vmid → 幂等检查 → GetVMConfig 读规格 → IP 复用/回退分配 → 单事务落库）；`ListUnmanagedVMs(ctx, nodeID)`（调 ListVMs + 过滤已托管）
- [ ] 3.2 IP 策略实现：解析 ipconfig0 静态 IP → 匹配池（CIDR 包含 + 白名单含节点 + 地址 free）→ `ClaimIPByAddressTx`；无匹配回退池分配
- [ ] 3.3 磁盘大小解析：遍历 config 磁盘键（scsi/ide/sata/virtio/efidisk/tpmstate）复用 `parseDiskSizeGB` 求和
- [ ] 3.4 VMService 接口与测试桩更新（mock 新增方法）；`service/vm_import_test.go` 覆盖：成功导入、重复导入 409、vmid 不存在 404、节点失败 503、静态 IP 复用命中、静态 IP 占用回退、DHCP 回退分配、池耗尽

## 4. handler 与路由

- [ ] 4.1 `api/handlers/vm_handler.go` 新增 `GET /vms/unmanaged`（node_id 查询参数）与 `POST /vms/import`；新错误码 `vm_already_managed`(409) / `vm_not_found_on_node`(404) 接入 `mapVMServiceError`
- [ ] 4.2 `vmResponse` 的 image_id/storage_type_id 加 omitempty；`api/router.go` 注册新路由
- [ ] 4.3 handler 单元测试（router_test 或 handler 测试）

## 5. OpenAPI 契约（红线）

- [ ] 5.1 `docs/openapi.yaml`（权威源）：新增 listUnmanagedVMs / importVM 两个 operation + 请求/响应 schema（UnmanagedVM、ImportVMRequest）+ 错误码；VMResponse image_id/storage_type_id 改可空
- [ ] 5.2 `api/swagger/openapi.yaml`（挂载副本）同步；`go test ./...` 与契约校验通过

## 6. 前端

- [ ] 6.1 `web/app/api/vms.ts` 新增 `listUnmanagedVMs`/`importVM`；`web/app/api/types.ts` 与 `schema.d.ts` 同步可空字段与新类型
- [ ] 6.2 `web/app/pages/vms.vue` 新增「导入 VM」弹窗：选可用区 → 节点 → 候选 VM（加载未托管列表）→ 提交；失败展示后端错误

## 7. e2e 与验证

- [ ] 7.1 `e2e/e2e_test.go` fake PVE 增加预置 VM 能力（registerVM，含 ipconfig0）；新增导入场景：预置 VM → 导入 → GET /vms 出现且状态透传 → start/stop 可用 → destroy 清理
- [ ] 7.2 全量验证：`go vet`、`go test ./...`、e2e（SPARK_E2E_DSN）、前端 build/lint

## 8. 审查与收尾

- [ ] 8.1 `reviewer` 代码审查并修复问题；`security-reviewer` 安全审查（IP 领取竞态、23505 兜底、错误消息脱敏）
- [ ] 8.2 openspec sync-specs 同步主规格、archive 归档变更
