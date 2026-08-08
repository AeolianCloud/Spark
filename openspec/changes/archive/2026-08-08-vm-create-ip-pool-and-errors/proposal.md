## Why

创建虚拟机时，后端按「区域 IP 池 → 池白名单∩启用节点 → 镜像过滤 → 可达性探测」调度节点并分配 IP。但当区域没有 IP 池、或池白名单为空（且节点带镜像）时，调度链落到兜底分支，报出误导性的 `node_unavailable`（"no reachable node with image"），实际问题是"无可用 IP 池"，运维无法据此定位。同时前端创建 VM 表单没有 IP 池选择能力，用户无法控制/感知 IP 池配置，遇到该错误时也无从下手。

## What Changes

- **后端新增错误码 `no_available_ip_pool`（400）**：区域没有 IP 池、或全部池的白名单节点与启用节点交集为空（无可调度候选）时返回该错误，消息明确指出区域与原因；`node_unavailable` 仅保留给"有池有候选但节点不可达/探测失败"的场景。
- **后端创建 VM 支持可选指定 IP 池**：`POST /vms` 请求体新增可选 `pool_id`；不传时保持现有自动遍历选池逻辑，传了则只在该池内调度（池不存在/不属于该区域 → 404；池白名单为空 → `no_available_ip_pool`）。响应不变（分配的 IP 仍由系统决定，前端不可选 IP 地址本身）。
- **错误优先级的语义收窄**：`image_not_available_in_zone` 的触发条件收窄为"存在可用 IP 池候选但无镜像"；无池/全池无候选时（无论镜像状态）一律返回 `no_available_ip_pool`（池配置是创建的前置条件，优先暴露）。相关 spec、api-errors.md 触发场景描述同步修订。
- **前端创建 VM 表单新增 IP 池下拉（可选）**：选中 Zone 后联动加载该 Zone 的 IP 池，未选择时提交不带 `pool_id`（后端自动选池）；选项为空时表单提示"该区域未配置 IP 池"并给出配置引导。
- **契约同步**：`docs/openapi.yaml` 与 `api/swagger/openapi.yaml` 双副本更新（`pool_id` 参数、`no_available_ip_pool` 错误码、响应描述）；`docs/api-errors.md` 新增错误码说明；前端 API client（`web/app/api/`）同步生成。

## Capabilities

### New Capabilities

- 无（不引入新能力域，行为变更落在既有能力内）

### Modified Capabilities

- `vm-lifecycle`: 创建虚拟机支持可选指定 IP 池（`pool_id`），并新增「区域无可用 IP 池」与「节点不可达」的区分场景
- `ip-pool`: IP 池的随机分配语义从"后端自动遍历"扩展为"创建 VM 时可显式指定池（可选，默认自动）"
- `web-management-ui`: VM 创建表单新增 IP 池下拉（可选、联动 Zone），并展示"区域未配置 IP 池"的引导提示

## Impact

- **后端**：`service/vm_service.go`（`selectPoolAndNode` 签名与错误分支、`CreateVM` 校验、`KindNoAvailableIPPool = 110` 与构造器）、`api/handlers/vm_handler.go`（`mapVMServiceError` 新 case + `CodeNoAvailableIPPool` 常量）、`service/vm_service_test.go`（新增用例；`TestSelectPoolAndNodeSkipsUnreachablePools` 第三段断言语义反转：无池 → no_available_ip_pool）
- **契约**：`docs/openapi.yaml`、`api/swagger/openapi.yaml`（`POST /vms` 增加 `pool_id`，错误响应增加 `no_available_ip_pool` 并同步 ErrorDetail enum，`node_unavailable` 场景描述收窄）、`docs/api-errors.md`（新增错误码登记；按"新增错误码属破坏性变更"规则登记并说明当前无 API 版本化机制、与既有新增错误码同例随 PR 登记）
- **前端**：`web/app/pages/vms.vue`（创建表单加池下拉 + Zone 切换重置 pool_id）、`web/app/api/`（client/types 同步）、`web/app/api/contract-verify.ts`（`_AssertCreateVMReq`/`_AssertCreateVMReqRequired` 断言更新，否则 `npm run typecheck` 失败）、`web/app/api/pools.ts`（复用既有 `listPools({zone_id})`，无需新方法）
- **测试**：`e2e/e2e_test.go` 既有断言不受影响（镜像场景已配池）；新增 e2e 用例（区域无池创建 VM → 400 no_available_ip_pool）；`go test ./...` 与 `npm run api:check` 全绿
