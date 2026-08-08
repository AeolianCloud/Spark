## Why

VM 供给失败（status=failed，provision_error 已落库）时，前端销毁按钮被 `canDestroyVM` 一并禁用（`isProvisioningStatus` 把 creating/failed 统一视为过渡态），运维无法在前端删除供给失败的 VM，只能手工调 API。failed 与 creating 语义不同：creating 是供给进行中（PVE 侧可能正在创建实体，销毁有竞态）；failed 是供给已终态失败（PVE 侧可能无实体或残留半成品），删除是安全的且是运维刚需。

## What Changes

- 前端 `web/app/utils/vm.ts`：`canDestroyVM` 对 `failed` 状态放行（`creating` 仍禁用）；`isProvisioningStatus` 的语义拆分——`creating` 为「供给中」禁用全部操作，`failed` 为「供给失败」仅放行销毁，启停/重启/调整规格对 `failed` 仍禁用（无 PVE 实体可操作，后端 `vm_not_ready` 兜底）。
- 组件与页面：`VmActions.vue` 销毁按钮随 `canDestroyVM` 自动放行；`vms.vue` 列表页与 `vms/[id].vue` 详情页销毁入口随之可用；详情页 292 行附近「creating/failed 状态禁用操作」注释同步更新。
- 后端行为不变（已实测 failed 销毁 204 成功，`destroyLocal` 对 `pve_vmid == 0` 跳过 PVE 调用仅清理本地记录并释放 IP），在 spec 中写明销毁 failed VM 的语义与已存在测试的覆盖。
- 前端校验：`npm run typecheck`；当前 `web/app/utils/` 下无单元测试文件（已确认无 `__tests__` 与 `*.test.ts`），如后续新增则同步覆盖 `canDestroyVM`。

## Capabilities

### New Capabilities

- 无

### Modified Capabilities

- `vm-lifecycle`: 「生命周期操作」Requirement 销毁相关场景新增「销毁供给失败的虚拟机」——failed 且 `pve_vmid == 0` 时幂等清理本地行并释放 IP，有残留实体时 purge 清理；该行为当前仅由既有实现与测试（`TestDestroyUnprovisioned`）隐含，需显式化到 spec。
- `web-management-ui`: 「VM 生命周期操作」Requirement 新增供给失败（failed）状态下仅销毁操作可用的场景说明。

## Impact

- 前端：`web/app/utils/vm.ts`（`isProvisioningStatus`/`canOperateVM`/`canDestroyVM` 及启停/重启/调规格判定的语义拆分）、`web/app/components/VmActions.vue`（无代码改动，随判定函数自动放行）、`web/app/pages/vms/[id].vue`（292 行附近注释更新）。
- 后端：无代码改动。`service/vm_service.go` `destroyLocal`（line 1361）对 failed 已支持：`pve_vmid == 0` 跳过 PVE 调用、单事务内释放 IP 并删除行；`pve_vmid > 0` 时 PVE DestroyVM（purge=true），404 视为已销毁继续本地清理，其余 PVE 失败保留记录与 IP。既有测试 `TestDestroyUnprovisioned`（vm_service_test.go:2073）已覆盖 `pve_vmid == 0` 路径。
- 契约：无 API 变更（DELETE /vms/:id 已放行 failed），无需修改 `docs/openapi.yaml`。
- 测试：前端 `npm run typecheck`；后端无新增测试需求（既有 `TestDestroyUnprovisioned` 已覆盖），如需可补 failed 状态下销毁的显式测试。
