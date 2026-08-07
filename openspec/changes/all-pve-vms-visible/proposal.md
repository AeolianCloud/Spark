## Why

当前 `GET /vms` 只展示本地数据库中有记录的虚拟机，PVE 节点上已有的虚拟机必须手动逐台"导入"后才可见、可控制。用户期望以 PVE 为唯一权威数据源：连接节点后即可看到该节点上的**全部**虚拟机（含外部已有），非 Spark 创建的仅需标识来源即可直接控制，且所有操作必须留存记录。

## What Changes

- `GET /vms` 列表合并：PVE 节点上存在、本地无记录的虚拟机并入列表，标识为 `external`（未认领）。**BREAKING**：响应新增 `source` 字段（`spark_created` / `claimed` / `external`），前端需配合展示徽章
- 生命周期控制放开：start/stop/reboot/destroy 不再强制要求本地有 VM 记录，外部虚拟机凭 `node_id + pve_vmid` 即可直接控制（PVE 原生 API 已证实只依赖这两个参数）
- 新增操作记录（审计日志）：所有生命周期操作（含外部虚拟机）落库留存，字段含节点、VMID、动作、结果、时间
- 导入语义调整为"认领"：外部虚拟机认领到本地账本，分配 IP 改为**可选**（虚拟机终端内可能自行修改 IP，不再强制从 IP 池分配）
- 节点故障/禁用时：该节点上的虚拟机不显示，并在列表/节点状态中标记"节点故障"
- OpenAPI 契约双副本（`docs/openapi.yaml` 与 `api/swagger/openapi.yaml`）同步更新；错误码变更同步 `docs/api-errors.md`
- **不在本次范围**：用户体系（单独提案，操作记录表预留 `user_id` 可空列）

## Capabilities

### New Capabilities
- `operation-log`: 虚拟机生命周期操作的审计记录能力，覆盖托管与未认领的外部虚拟机

### Modified Capabilities
- `vm-lifecycle`: 虚拟机列表可见性（并入未认领的外部虚拟机）、生命周期操作不再要求本地记录、节点故障时的显示策略
- `vm-import`: 导入语义调整为"认领"，IP 分配策略由强制改为可选

## Impact

- **后端**：`service/vm_query.go`（列表合并与 source 标识）、`service/vm_service.go`（生命周期放开本地记录强校验 + 操作记录落库）、`service/vm_import.go`（认领与 IP 可选）、`repository/`（新增操作记录表读写）、`database/migration/0008`（操作记录表、`vms.source` 或等价字段）
- **API**：`api/handlers/vm_handler.go` 列表响应与生命周期入口；`docs/openapi.yaml` 双副本；`docs/api-errors.md`
- **前端**：`web/app/pages/vms.vue`（来源徽章、外部虚拟机操作入口、节点故障标识）
- **测试**：`e2e/` fake PVE 扩展（外部虚拟机直接控制、操作记录断言）；单测
