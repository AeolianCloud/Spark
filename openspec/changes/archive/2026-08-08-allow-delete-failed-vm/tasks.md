## 1. 前端判定函数语义拆分

- [x] 1.1 `web/app/utils/vm.ts`：`isProvisioningStatus` 收敛为仅判定 `creating`，新增 `isFailedStatus` 判定（status === 'failed'），同步更新顶部「过渡状态」注释
- [x] 1.2 `web/app/utils/vm.ts`：`canOperateVM` 改为排除 `creating` 与 `failed`（等价于仅放行 running/stopped/paused/suspended/未知状态）
- [x] 1.3 `web/app/utils/vm.ts`：`canDestroyVM` 对 failed 放行（`canOperateVM(status) || isFailedStatus(status)`），`canStartVM`/`canStopVM`/`canRestartVM`/`canResizeVM` 保持基于 `canOperateVM` 不变
- [x] 1.4 更新 vm.ts 中「生命周期操作可用性」区段注释：creating/failed 禁用启停/重启/调规格，failed 可销毁

## 2. 页面与组件

- [x] 2.1 确认 `VmActions.vue` 销毁按钮随 `canDestroyVM` 自动放行，无需改动（`disabled = busy || busyAny || !canDestroyVM(status)` 已自动生效）
- [x] 2.2 `web/app/pages/vms/[id].vue` 292 行附近注释「creating/failed 状态禁用操作」更新为「creating/failed 禁用启停/重启/调规格（后端 vm_not_ready 兜底）；failed 可销毁」
- [x] 2.3 确认 `vms.vue` 列表页销毁入口随 `canDestroyVM` 自动放行，无需改动（VmActions 共用）

## 3. 验证

- [x] 3.1 `web/` 下运行 `npm run typecheck` 通过
- [x] 3.2 确认 `web/app/utils/` 无既有测试文件需同步（当前无 `__tests__` 与 `*.test.ts`）；如存在则补充 `canDestroyVM` 对 failed/creating 的断言
- [x] 3.3 后端无代码改动：确认 `destroyLocal`（vm_service.go:1361）对 failed 且 `pve_vmid == 0` 的路径既有测试 `TestDestroyUnprovisioned` 已覆盖，不新增后端测试
