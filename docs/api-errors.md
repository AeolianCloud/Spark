# API 错误码契约

本页是错误响应的唯一权威契约，与 `api/handlers/error.go`、`api/handlers/zone_handler.go`、`api/handlers/vm_handler.go` 中的错误码常量保持一致。修改任何错误码必须同步更新本页。

## 错误响应结构

所有错误响应统一为：

```json
{"error": {"code": "not_found", "message": "vm 42 not found"}}
```

- `code`：稳定的机器可读错误码（见下方清单）。程序解析**只允许依赖 `code`**。
- `message`：仅供人阅读的描述，**不可被程序解析**——文案可能随版本措辞调整，且不保证稳定的格式或参数位。

## x-ms-error-code 响应头

业务 API 的错误响应（统一错误结构，含未匹配路由的 404 与 405 兜底）都携带 `x-ms-error-code` 响应头，其值与响应体中的 `code` 一致：

```http
HTTP/1.1 404 Not Found
x-ms-error-code: not_found

{"error": {"code": "not_found", "message": "vm 42 not found"}}
```

网关、负载均衡与客户端拦截器可以只读取该响应头判断错误类型，无需解析响应体。

`/healthz` 是探活端点，不属于业务 API：其 200 正常响应不携带本头；503 degraded 状态响应携带 `x-ms-error-code: service_unavailable` 头（激活下方清单中预留的 `service_unavailable` 码），便于探活器与负载均衡统一判断服务不可用状态。

## 契约稳定性

- 错误码是 API 契约的一部分：**不可随意变更**。错误码的取值与其对应的 HTTP 状态码一旦发布，语义即固定。
- **新增错误码属于破坏性变更**，必须伴随 API 版本升级，并在本页登记。
- 修订 `message` 文案不视为破坏性变更。
- 标注「未使用（预留）」的错误码虽当前无路由触发，仍是契约的一部分，不应删除或改写用途。

## 错误码清单

| 错误码 | HTTP 状态 | 含义 | 触发场景 |
| --- | --- | --- | --- |
| `bad_request` | 400 | 请求参数非法 | 请求体无法解析、必填参数缺失、路径/查询参数格式错误 |
| `not_found` | 404 | 资源不存在 | 引用不存在的 zone / node / ip-pool / storage-type / image / VM，或路径未匹配任何路由 |
| `method_not_allowed` | 405 | 请求方法不允许 | 路径存在但请求方法未注册（响应携带 Allow 头列出允许的方法） |
| `conflict` | 409 | 与现有状态冲突 | 重名（zone / storage-type / image 等唯一约束）、IP 池网段重叠、删除仍被引用的资源 |
| `unprocessable_entity` | 422 | 语义正确但被业务规则拒绝 | 未使用（预留）；磁盘缩小走专门的 `disk_shrink_not_allowed` |
| `internal_error` | 500 | 服务器内部错误 | 未归类异常的统一兜底，不暴露内部细节 |
| `service_unavailable` | 503 | 服务不可用 | `/healthz` 数据库探活失败时的 degraded 状态（业务 API 暂无触发） |
| `dependency_failed` | — | 依赖子系统失败 | 未使用（预留），已定义、暂未接线 |
| `node_unavailable` | 503 | 无可用的 PVE 节点 | 创建 VM 时所有候选节点不可达或被禁用 |
| `ip_exhausted` | 409 | IP 池无空闲地址 | 所有候选 IP 池的地址均已分配完毕 |
| `vm_not_ready` | 409 | VM 尚不可操作 | 供给未完成或 PVE 侧 VM 已不存在时执行生命周期操作 |
| `disk_shrink_not_allowed` | 422 | 磁盘不支持缩小 | resize 请求中 `disk_gb` 小于当前磁盘大小 |
| `image_not_available_in_zone` | 400 | 镜像在该区域不可用 | 请求的镜像未同时存在于该区域全部启用节点 |
| `vm_not_found_on_node` | 404 | 节点 PVE 可达但 VM 不在该节点上 | 认领（import）不存在的 pve_vmid，或对 ext- 标识指向的已移除/不存在的 VM 执行生命周期操作（区别于 zone/node 自身不存在的 `not_found`） |
| `vm_already_managed` | 409 | 该节点上的 pve_vmid 已被托管 | 重复认领同一 PVE VMID（区别于一般资源冲突的 `conflict`） |
| `invalid_vm_id` | 400 | VM id 无法解析 | 生命周期操作/操作记录查询的 `:id` 既非正整数也非 `ext-{nodeID}-{vmid}` 合成标识 |
| `operation_log_failed` | 500 | 操作记录写入失败 | PVE 已受理生命周期操作但审计记录落库失败（前端可刷新确认） |
