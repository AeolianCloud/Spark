## Context

现状：前端 `web/app/utils/vm.ts` 中 `isProvisioningStatus` 将 `creating` 与 `failed` 统一视为过渡态，`canOperateVM` 取反后一并禁用启停/重启/调规格/销毁；`canStartVM`/`canStopVM`/`canRestartVM`/`canResizeVM`/`canDestroyVM` 均基于 `canOperateVM`（vm.ts:84-111）。`VmActions.vue` 销毁按钮 `disabled = busy || busyAny || !canDestroyVM(status)`（VmActions.vue:116），列表页（vms.vue:640）与详情页（vms/[id].vue:315）共用该组件，因此只要 `canDestroyVM` 放行 failed，两页销毁入口即自动可用。

后端对 failed 销毁已天然放行：`destroyLocal`（vm_service.go:1361）不检查 status，`pve_vmid == 0` 时跳过 PVE 调用、单事务内释放 IP（`ReleaseIPByVMTx`）并删除行（`DeleteVMTx`）；`pve_vmid > 0` 时先 `DestroyVM(purge=true)`，PVE 404 视为已销毁继续本地清理，其余 PVE 失败保留记录与 IP。`TestDestroyUnprovisioned`（vm_service_test.go:2073）已覆盖 `pve_vmid == 0` 路径。动机见 proposal.md - Why。

## Goals / Non-Goals

**Goals:**
- 前端对 failed 状态的 VM 放行销毁操作（creating 仍禁用），保持启停/重启/调规格对 failed 禁用
- 语义拆分：failed 与 creating 不再共用同一「过渡态」判定
- 后端行为不变，仅将既有销毁语义显式化到 spec

**Non-Goals:**
- 不修改后端代码、不修改 API 契约（DELETE /vms/:id 已放行 failed）
- 不改变 creating 状态的操作禁用行为
- 不新增前端单元测试框架（当前 utils 无测试文件）

## Decisions

### D1：isProvisioningStatus 语义拆分

将 `isProvisioningStatus` 收敛为仅判定 `creating`（供给进行中），新增独立判定 `isFailedStatus`（status === 'failed'）。理由：两个状态的操作面不同——creating 期间 PVE 侧可能正在创建实体，销毁有竞态故全禁；failed 是终态失败，删除安全。`canOperateVM` 改为 `!isProvisioningStatus && !isFailedStatus`（等价于仅放行 running/stopped/paused/suspended/未知状态）。

### D2：仅销毁放行 failed

`canDestroyVM` 改为 `canOperateVM(status) || status === 'failed'`（或等价形式）；`canStartVM`/`canStopVM`/`canRestartVM`/`canResizeVM` 保持基于 `canOperateVM` 不变。取舍：failed 的 VM 无（或只有残留半成品）PVE 实体，启停/重启/调规格无对象可操作，后端 `vm_not_ready` 会兜底返回 409；仅销毁有实际意义（清理本地记录、释放 IP、purge 残留）。若对 failed 放行全部操作会引入「对无实体 VM 发起启动」的无意义交互。

### D3：既有调用方零改动

`VmActions.vue`、`vms.vue`、`vms/[id].vue` 均通过 `canDestroyVM` 判定销毁可用性，无需组件级改动；详情页 292 行附近注释「creating/failed 状态禁用操作」需同步更新为「creating/failed 禁用启停/重启/调规格；failed 可销毁」。

### D4：后端销毁语义显式化

`destroyLocal` 对 failed 的行为已符合 spec 预期：`pve_vmid == 0` 幂等清理（跳过 PVE、释放 IP、删行），`pve_vmid > 0` 且 PVE 404 时视为已销毁继续清理，其余 PVE 失败保留记录与 IP 供重试。spec 的「销毁供给失败的虚拟机」场景即是对该既有行为的固化，不新增后端代码；如需增强，可补充 failed 状态下销毁的显式测试（与 `TestDestroyUnprovisioned` 同构）。

## Risks / Trade-offs

- [销毁 failed VM 时 PVE 侧残留实体销毁失败（非 404）] → 既有行为已兜底：保留本地记录与 IP，返回错误，运维可重试；不因本次变更改变。
- [前端判定与后端放行不一致（如未来后端对 failed 加限制）] → 销毁按钮放行后若后端拒绝，界面展示后端错误信息（既有场景「操作失败」覆盖），VM 状态不变。
- [unknown 状态语义：`canDestroyVM` 对未知状态本就放行，拆分后不回归] → `canOperateVM` 对未知状态仍返回 true，行为与现状一致。

## Migration Plan

纯前端判定函数语义调整 + 注释更新，无数据迁移、无部署顺序要求；前端发版后销毁按钮即对 failed 可用。后端无需变更。
