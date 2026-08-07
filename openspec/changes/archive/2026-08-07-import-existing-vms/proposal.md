## Why

添加真实 PVE 节点后，节点上已有的虚拟机（在 PVE 界面手工创建的）不会出现在 Spark 的 VM 列表中——列表只合并本地元数据，PVE 上无本地记录的 VM 被刻意跳过（设计 D1 的透传模型）。用户需要把这些已有 VM 纳入管理（纳管），获得完整的生命周期管理能力。

## What Changes

- 新增 **VM 导入（纳管）**：将 PVE 节点上已有的 VM 导入为 Spark 托管的 VM，导入后支持完整的生命周期操作（启动/停止/重启/销毁/升降配）与透传状态查询
- 新增未托管 VM 候选查询：按节点列出该节点 PVE 上尚未被 Spark 托管的 VM，供导入入口选择
- IP 策略：**优先复用 PVE 配置（ipconfig0）中的静态 IP**——静态 IP 属于某池且空闲时直接占用；VM 无静态 IP（DHCP）或无匹配池时回退从池分配新 IP
- 导入不修改 PVE 侧任何配置，仅建立本地管理记录；导入的 VM 无镜像与存储类型关联、无 cloud-init 密码
- 数据库 schema 变更：`vms.image_id`/`storage_type_id`/`password_encrypted` 允许 NULL；新增 `(node_id, pve_vmid)` 部分唯一索引（pve_vmid > 0）防止重复导入。**BREAKING**：`VMResponse.image_id`/`storage_type_id` 变为可空
- 前端 VM 列表页新增「导入 VM」入口：选可用区 → 节点 → 候选 VM → 导入

## Capabilities

### New Capabilities

- `vm-import`: 将 PVE 节点上已有的 VM 纳管为托管 VM，含未托管候选查询、静态 IP 复用与导入幂等性

### Modified Capabilities

- `vm-lifecycle`: 虚拟机生命周期新增"导入虚拟机"入口行为——导入后的 VM 与创建产生的 VM 享有同等生命周期能力；虚拟机的镜像/存储类型关联变为可空
- `web-management-ui`: VM 列表页新增"导入 VM"操作入口，含可用区/节点/候选 VM 选择流程

## Impact

- **数据库**：新增 migration 0007（放宽 NOT NULL、新增部分唯一索引）
- **后端**：`model.VM`（ImageID/StorageTypeID 可空）、`repository/vm_repo.go`（GetVMByNodeVMID/ImportVMTx）、`repository/ip_pool_repo.go`（按地址精确领取）、`service`（ImportVM、未托管列表）、`api/handlers/vm_handler.go`（GET /vms/unmanaged、POST /vms/import）、`api/router.go`
- **API 契约**：`docs/openapi.yaml` 与 `api/swagger/openapi.yaml` 同步新增两个端点与错误码（409 vm_already_managed、404 vm_not_found_on_node），VMResponse 可空字段
- **前端**：`web/app/pages/vms.vue` 导入弹窗、`web/app/api/vms.ts`、`web/app/api/types.ts`
- **测试**：service/repository 单元测试；e2e fake PVE 预置 VM 的导入全链路
