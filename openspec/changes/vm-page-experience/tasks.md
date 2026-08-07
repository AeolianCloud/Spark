## 1. Phase 1：后端 + 契约（external 详情端点）

- [x] 1.1 `service/vm_query.go`：GetVM 支持 external 合成标识 `ext-{nodeID}-{vmid}` 解析，单查该节点 PVE 实时状态，复用 `externalVMListItem` 构建返回（含实时指标）；错误映射节点故障 → 503 `node_unavailable`、VM 已移除 → 404 `vm_not_found_on_node`
- [x] 1.2 `api/handlers/vm_handler.go`：Get 改为接受字符串 id（数字走原路径，ext- 正则校验非法 400）；清理 `setLifecycleLocation` 对 ext- 的省略逻辑
- [x] 1.3 单元测试：service 层（ext- 解析成功/节点故障/VM 不存在/非法格式）；handler 层（ext- 200、非法 ext- 400）
- [x] 1.4 `e2e/`：fake PVE 场景新增 GET /vms/ext-{node}-{vmid} 成功/404/503 用例，`go test -tags=e2e ./e2e/ -count=1 -v` 通过
- [x] 1.5 `docs/openapi.yaml` 与 `api/swagger/openapi.yaml` 双副本：`GET /vms/{id}` 的 id 改为 oneOf（integer | string ext- pattern）；`npx --yes @redocly/cli lint docs/openapi.yaml` 通过、双副本字节一致
- [x] 1.6 `web/app/api/generated` 重新生成 client，`npm run api:check` 通过（git diff 为空）

## 2. Phase 2：前端（操作反馈 + external 详情页 + 记录按钮移除）

- [x] 2.1 `web/app/utils/vm.ts`：过渡状态徽章映射（启动中/关闭中/重启中）与 pending 判定工具
- [x] 2.2 `web/app/components/VmActions.vue`：四按钮恒显 + 禁用口径（disabled = busy || busyAny || !canXxx）、新增 `pendingAction` prop 定向 loading
- [x] 2.3 新增 `web/app/composables/useVMPendingAction.ts`：3s 观察轮询（getVM 单查）+ D2 判定表 + 90s 超时兜底 + 两段式 toast（列表页与详情页复用）
- [x] 2.4 `web/app/pages/vms.vue`：行内操作接入 pending 状态与观察轮询；external 行新增「详情」按钮（/vms/ext-{node}-{vmid}）
- [x] 2.5 `web/app/pages/vms.vue`：移除「记录」按钮、操作记录弹窗及相关状态/函数/import
- [x] 2.6 `web/app/pages/vms/[id].vue`：路由参数兼容 ext- 标识；external 时本地字段展示「—」+ 未纳管提示条；认领入口（复用 claim modal 逻辑）；操作观察接入
- [x] 2.7 验证：`npm run lint` + `npm run typecheck` 通过；后端 `go test ./...` 通过；浏览器手动验证列表/详情两页交互
