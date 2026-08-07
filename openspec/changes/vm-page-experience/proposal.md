## Why

VM 页面存在三个体验问题：① 开机/关机状态下操作按钮数量不一致；② 生命周期操作无持续反馈——点击后"受理"一闪而过，异步执行窗口（PVE 启动/关闭/重启需几十秒）内用户无感知，成败提示弱；③ 未纳管（external）VM 无法查看详情——该能力 PVE 原生具备（config/status 均可查询），认领只是"纳入托管"，不应阻塞"查看"。

## What Changes

- VM 生命周期操作按钮改为**恒显 + 禁用**口径：启动/关闭/重启/销毁在任何状态下数量一致，不可用操作灰色禁用，仅触发中的按钮展示转圈。
- 引入**本地过渡状态 + 观察轮询**：操作受理后目标按钮转圈禁用，3s 间隔单查目标 VM（`getVM`），直至 PVE 状态确认生效（启动→running、关闭→stopped、重启→uptime 重置或状态变迁），90s 超时按当前状态兜底提示。
- **受理 + 结果双 toast**：点击即提示"正在执行"，结果到达后按成功/失败/超时分别提示；失败保留行内错误条。
- **GET /vms/{id} 支持 external 合成标识** `ext-{nodeID}-{vmid}`：external 条目获得详情页（实时状态 + 规格查看、生命周期操作、认领入口），不开放调整规格（保持未纳管语义）。
- 移除列表页「记录」按钮与操作记录弹窗；后端 `GET /vms/:id/operations` 端点与 `vm_operations` 审计表**保留**（未来统一日志系统的数据基础），契约不变。

## Capabilities

### New Capabilities

- `vm-operations-feedback`: 生命周期操作的前端持续反馈行为（按钮一致性、转圈执行状态、双 toast 提示）。

### Modified Capabilities

- `vm-lifecycle`: 穿透式状态查询扩展——详情查询支持未纳管（external）虚拟机的合成标识。
- `web-management-ui`: 界面为未纳管虚拟机提供详情页（查看 + 操作 + 认领，无调整规格）；操作记录不再作为独立界面入口。

## Impact

- 后端：`api/handlers/vm_handler.go`（Get 接受字符串 id）、`service/vm_query.go`（GetVM 支持 ext- 标识）、相关单测、`e2e/`（fake PVE 场景）。
- 契约：`docs/openapi.yaml` 与 `api/swagger/openapi.yaml` 双副本同步（`GET /vms/{id}` 的 id 改为 oneOf integer/ext- 字符串）；`web/app/api/generated` 客户端重新生成。
- 前端：`web/app/components/VmActions.vue`、`web/app/pages/vms.vue`、`web/app/pages/vms/[id].vue`、`web/app/utils/vm.ts`、新增 `web/app/composables/useVMPendingAction.ts`。
- 无新增错误码（`vm_not_found_on_node` 已存在）；无破坏性契约变更。
