## 1. 数据库迁移

- [x] 1.1 新增迁移 0008：`vms` 表加 `source` 列（默认 `spark_created`），存量 `image_id IS NULL` 行回填 `claimed`
- [x] 1.2 新增迁移 0008：建 `vm_operations` 表（node_id/pve_vmid/action/result/error_message/user_id 可空/created_at）及 `(node_id, pve_vmid, created_at DESC)` 索引
- [x] 1.3 model 层：`VM` 加 `Source` 字段，新增 `VMOperation` 实体

## 2. 列表合并（external 条目 + 来源标识）

- [x] 2.1 `vm_query.go`：本地查询改为全量行，与每节点 PVE 全量摘要 join，生成三类条目（本地行+PVE、本地行-only、PVE-only external）
- [x] 2.2 external 条目：合成 id `ext-{nodeID}-{vmid}`、uuid/created_at 空、name/规格/状态取 PVE 摘要
- [x] 2.3 列表按 `(node_id, pve_vmid)` 排序后内存分页，X-Total-Count 返回合并后总数
- [x] 2.4 响应条目增加 `source` 字段（spark_created/claimed/external），含单测覆盖三类条目与分页稳定性

## 3. 生命周期操作放开 + 操作记录

- [x] 3.1 `vm_service.go` 路由改造：数字 id 走现有本地行路径；`ext-` 前缀反查 zone/node 直调 PVE（start/stop/reboot）
- [x] 3.2 destroy 支持 external：PVE destroy，无本地行/IP 清理；本地行 destroy 流程不变
- [x] 3.3 操作记录落库：PVE 受理成功后同步写 `vm_operations`，写失败返回明确错误码；四个动作全覆盖（含 external）
- [x] 3.4 操作记录查询接口 `GET /vms/{id}/operations`（按时间倒序分页，支持数字 id 与 ext- 标识），handler + service + repo + 单测
- [x] 3.5 错误码：操作记录写失败新增/复用错误码，同步 `docs/api-errors.md`

## 4. 认领改造（原导入）

- [x] 4.1 `vm_import.go`：`POST /vms/import` 请求体新增可选 `ip` 字段（从区域池分配），不传则 `ip_id` 置空，移除强制分配/静态 IP 复用逻辑
- [x] 4.2 认领成功后 `source` 置为 `claimed`
- [x] 4.3 下线 `GET /vms/unmanaged`：handler 路由删除、`ListUnmanagedVMs` 移除、相关错误码清理，单测同步

## 5. 契约同步

- [x] 5.1 `docs/openapi.yaml`：列表响应加 `source`、external 条目说明、operations 新端点、import 请求体 `ip` 可选、下线 unmanaged，operationId 完整
- [x] 5.2 同步副本 `api/swagger/openapi.yaml`，双副本字节一致；`npx --yes @redocly/cli lint docs/openapi.yaml` 通过

## 6. 前端改造

- [x] 6.1 `vms.vue`：来源徽章（spark_created/claimed/external 三态）、external 条目可执行生命周期操作、节点故障 warning 渲染为醒目 banner
- [x] 6.2 移除 unmanaged 候选弹窗流程，认领入口基于 external 条目；认领表单 IP 可选
- [x] 6.3 操作记录展示（详情页或列表行内入口），`npm run api:check` 通过（生成的 client 与提交版本 git diff 为空）

## 7. e2e 与回归

- [x] 7.1 e2e：fake PVE 扩展——external VM 直接 start/stop/reboot/destroy 断言、操作记录查询断言、认领 IP 可选断言、unmanaged 接口下线断言
- [x] 7.2 `go test -tags=e2e ./e2e/ -count=1 -v` 全量通过；`go vet ./...` 与 `gofmt` 检查
