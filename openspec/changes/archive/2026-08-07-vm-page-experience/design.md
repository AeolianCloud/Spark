## Context

PVE 生命周期操作是"202 受理"模型：请求秒回、PVE 后台任务执行几十秒。现有前端只在 HTTP 在途窗口（毫秒级）展示按钮 loading，异步执行窗口无任何反馈。restart 调 PVE `status/reboot`（qm reboot = 软重启，先优雅关闭再启动，见 `pve/qemu.go:154`），停机窗口取决于 guest 关闭速度，不可预测。详情与列表契约均透传 uptime（`openapi.yaml:1828`，kvm 进程运行时长，单调递增）。external（未纳管）条目 id 为合成标识 `ext-{nodeID}-{vmid}`，现有 `GET /vms/{id}` 经 `parseIDParam` 仅接受数字 id（`api/handlers/vm_handler.go:249`），PVE 侧数据源完备（`VMStatus` 摘要 + `GetVMConfig`）。

## Goals / Non-Goals

**Goals:**
- 操作按钮数量恒定（恒显 + 禁用），仅触发按钮转圈
- 操作转圈持续到 PVE 状态确认生效（观察轮询），90s 超时兜底
- 受理 + 结果两段 toast；失败保留行内错误条
- `GET /vms/{id}` 支持 ext- 标识，external 获得详情页（只读 + 生命周期操作 + 认领入口）
- 移除列表页「记录」按钮与操作记录弹窗（后端审计端点/表保留）

**Non-Goals:**
- 不开放 external 调整规格（PATCH 保持数字 only，保持未纳管语义）
- 不新建任何端点；`GET /vms/:id/operations` 端点与 `vm_operations` 表保持不变
- 不做统一日志系统（后续独立变更）

## Decisions

### D1 操作观察轮询：独立 3s 定时器，单查目标 VM

操作在途时启动独立的 3s 观察定时器，对目标 VM 走 `getVM` 单查（本地行与 external 行统一——external 单查依赖 D6 的详情端点）；整页 15s 自动刷新逻辑一行不动；操作结束（成功/失败/超时）即停。请求并发安全复用 fetchSeq 守卫模式。替代方案（操作在途时整页提速 3s）因会对 PVE 按节点扇出、且与"≥10s 轮询约束"冲突而被否决；external 无单查端点是该方案的旧依赖，D6 使其消失。

**路由切换/卸载防护**（审查 B1/G2/S1）：composable 提供 `stop()`（停观察 + 清 pending + 递增序号）；详情页路由参数变化时先 `stop()` 再拉取；run 的受理响应返回后校验 `disposed || pending.id !== id`，拦截组件卸载与路由切换（组件复用不卸载）下的迟到响应，杜绝旧 VM 数据覆盖新页面、空转轮询与 observeSeq 污染（新操作卡死）。轮询快照回写前同样校验目标一致。

### D2 完成判定表：无中间失败信号，超时按状态快照兜底

| 操作 | 成功信号（终止转圈 + success toast） | 失败信号 | 90s 超时兜底 |
| --- | --- | --- | --- |
| start | status → `running` | 无（无法区分排队/失败回退） | warning「启动较慢仍在执行」 |
| stop | status → `stopped` | 无（ACPI 关机 30~60s 常见） | warning「关闭较慢仍在执行」 |
| restart | ① `uptime` 下降 ② stopped→running 变迁 | 无（软重启优雅关闭耗时不可预测，不设停机容忍窗口） | 超时态 `stopped` → error「重启中断，VM 已停止」；超时态 `running` → warning「仍在执行」 |

替代方案"9s 停机容忍判失败"因 qm reboot 为软重启（优雅关闭慢是常态，非失败）被否决。

### D3 restart 判定原理：uptime 归零

qm reboot = shutdown + start，任何重启都是 kvm 进程级重建，新进程 uptime 从 0 重计时。uptime 单调递增，**任何下降即判重启完成，零容差**；不依赖 guest agent。uptime 快照缺失（如 stopped 期间省略）时由状态变迁信号②兜底。

### D4 按钮恒显口径与定向 loading

`VmActions.vue` 四按钮恒显，`disabled = busy || busyAny || !canXxx(status)`；新增 `pendingAction` prop，仅触发按钮 `:loading`（不再整组转圈）。列表行宽对齐：本地行与 external 行均为"详情 + 4 操作 = 5 按钮"（记录按钮移除后）。

### D5 两段式 toast 语义

- 点击受理：info「正在启动/关闭/重启…」
- 结果：success「已启动/已关闭/已重启」；error toast + 行内 AppErrorAlert 双处提示；超时 warning（按 D2 快照文案）
- 行内错误条语义保留（`actionError` 路径）

### D6 GET /vms/{id} 扩展 ext- 标识

- `parseIDParam` 保持数字 only（其他端点不动）；`Get` handler 改为裸字符串：数字 → 原路径；否则 `^ext-([1-9]\d*)-([1-9]\d*)$` 校验，非法 400
- service 层新增 external 分支：解析 nodeID+vmid → 查该节点 PVE 实时状态 → 复用 `externalVMListItem` 构建（uuid/ip/created_at 空串、含实时指标）
- 错误映射：节点行缺失/禁用/调用失败 → 503 `node_unavailable`（不伪造状态）；VM 已从 PVE 移除（含模板）→ 404 `vm_not_found_on_node`（错误码已存在，无新增）
- **ext- 指向已托管 VM**（审查 G1）：先按 (nodeID, vmid) 查本地托管记录，命中则返回本地形态（数字 id、uuid/ip 齐全），与列表差集与生命周期 `resolveVMTarget` 语义一致；未命中才走 external 形态
- **错误消息截断**（安全审查）：所有 PVE 错误消息（详情 503、列表 warnings、resolveVMTarget 等）经公共 `truncatePVEErrorMsg`（500 rune）截断，杜绝 UpstreamError Body 全文（最大 1MiB）外泄
- 连带修正：`setLifecycleLocation` 对 ext- 省略 Location 的逻辑移除（Location 现在可用）
- 契约形态：`PathVMID` 与 `PathVMRef` 统一为单 string pattern `^([1-9][0-9]*|ext-[1-9][0-9]*-[1-9][0-9]*)$`

### D7 external 详情页能力边界

详情页对 external 开放：实时状态 + 规格查看、生命周期操作、认领入口（复用列表页 claim modal 逻辑，抽为可复用组件）；本地字段（UUID/IP/创建更新时间）展示「—」；无调整规格、无操作记录入口。

### D8 记录按钮移除

列表页删除「记录」按钮、操作记录弹窗及相关状态/函数/import；后端 operations 端点与审计表保留（统一日志系统未来复用）。

## Risks / Trade-offs

- [慢机器启动超 90s] → 超时兜底为 warning 非 error，文案"仍在执行"，不误导为失败；用户可手动刷新
- [重启快、3s 轮询捕不到 stopped 窗口] → uptime 归零信号覆盖（D3），无需状态变迁
- [uptime 穿透缺失（节点抖动）] → 观测中断时维持转圈继续等，超时兜底
- [详情页操作观察与页面刷新竞争] → 复用 fetchSeq 守卫，最后意图胜出
- [getVM 对 external 增加 PVE 单节点请求] → 每次仅 1 个 VM 的单节点查询，量级远小于整页扇出，可接受
