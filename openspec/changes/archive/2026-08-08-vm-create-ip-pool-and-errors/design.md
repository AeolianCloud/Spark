## Context

现有调度链 `selectPoolAndNode`（service/vm_service.go:565）按「区域 IP 池 → 池白名单∩启用节点 → 镜像存在性过滤 → 可达性探测」选池选节点。失败分支只区分了两种语义（无镜像 `imageNotAvailablef` / 节点不可达 `nodeUnavailablef`），但"区域无池""池白名单无候选节点"这两种场景没有独立错误，全部落到 `node_unavailable` 兜底（vm_service.go:616/620），报出误导性的 "no reachable node with image"。创建 VM 请求体（`CreateVMRequest`）目前无 `pool_id` 字段；前端创建表单（web/app/pages/vms.vue）也无 IP 池控件。错误码机制见 service/errors.go 的 `Error` 结构（`code` + HTTP 状态 + `x-ms-error-code` 头），错误码清单在 docs/api-errors.md，契约在 docs/openapi.yaml。

## Goals / Non-Goals

**Goals:**
- 新增独立错误码 `no_available_ip_pool`（400），区分「区域无池 / 池无候选节点」与「节点不可达」
- `POST /vms` 支持可选 `pool_id`：指定则限定该池调度，缺省保持自动遍历
- 前端创建表单加联动 Zone 的可选 IP 池下拉与"未配置 IP 池"引导
- 契约双副本 + api-errors + 前端 client 同步

**Non-Goals:**
- 不改 IP 分配语义：IP 地址仍由后端随机分配，前端不可指定具体 IP
- 不做池的健康度/占用率展示（列表页已展示占用情况，不在本变更范围）
- 不引入池的删除/编辑（前端 ip-pools.vue 已注明契约缺口，属既有待办）

## Decisions

### 1. 新错误码 `no_available_ip_pool` 定位为 400 还是 503

选用 **400**。理由：该错误本质是区域配置缺失（无池、白名单空），属于请求针对的资源状态错误而非服务端依赖故障；与既有的 `image_not_available_in_zone`（400，镜像在区域不可用）语义平行，便于前端按同类规则处理。`node_unavailable`（503）保留给"有池有候选但探测失败"（真实的依赖故障）。
备选：503（视同依赖故障）——被否，因为节点是可达的，报 503 会误导运维排查网络层。

### 2. `pool_id` 校验时机与语义

`CreateVM` 在步骤 1（区域存在性检查）之后、`selectPoolAndNode` 之前校验：`pool_id` 非空时必须 `GetPool` 且 `pool.ZoneID == req.ZoneID`，不满足返回 404（not_found，复用 `notFoundf`，与"指定不属于该区域的池"场景一致）。通过后把 `pool_id` 传给 `selectPoolAndNode`：指定池时只遍历该池（不再遍历 `ListPoolsByZone`），池白名单为空直接返回 `no_available_ip_pool`（消息指明池 id 与区域）；缺省时保持遍历全部池。这是对 `selectPoolAndNode` 的签名扩展：`selectPoolAndNode(ctx, zoneID, image)` → `selectPoolAndNode(ctx, zoneID, image, poolID *int64)`。

### 3. 错误分支重构（vm_service.go:565-621）

按优先级重排失败语义，消除误导：
1. 池候选集合为空（指定池的白名单∩启用=空，或所有池都无候选，或区域无池）→ `no_available_ip_pool`，消息区分「区域无池」「池无候选节点」两种子因（消息内说明即可，不新增子错误码）。**该分支置于镜像检查之前**：池配置是创建 VM 的前置条件，即使镜像也未下载，也优先暴露池缺失
2. 有池有候选但无镜像（`len(volIDs)==0` 且无扫描失败）→ `image_not_available_in_zone`（现状，触发条件收窄：从"无启用节点有镜像"收窄为"有池候选但无镜像"）
3. 扫描失败/探测失败 → `node_unavailable`（现状，保留 503）

注意现有分支 616/620 都把"无镜像但存在扫描失败节点"归为 node_unavailable，本设计保持该语义不变。已知边界：混合场景「部分池有候选但无镜像 + 非白名单节点有镜像」仍落 node_unavailable（volIDs 是全节点扫描），保持现状不扩大本变更范围。

### 4. Kind 值与 handler 接线

新增 `KindNoAvailableIPPool ErrorKind = 110`（100-109 已占用：node_unavailable 100 / ip_exhausted 101 / vm_not_ready 102 / disk_shrink 103 / image_not_available 104 / vm_not_found_on_node 105 / vm_already_managed 106 / invalid_vm_ref 107 / operation_log_failed 108 / user_has_resources 109），构造器 `noAvailableIPPoolf` 放 vm_service.go（与 imageNotAvailablef 同域）。handler 侧 `mapVMServiceError`（vm_handler.go:50-73）新增 `case service.KindNoAvailableIPPool → NewError(400, CodeNoAvailableIPPool, ...)`，常量 `CodeNoAvailableIPPool = "no_available_ip_pool"`。

### 5. 前端表单与联动

复用 `vms.vue` 已有的 Zone→镜像联动模式（`watch(createForm.zone_id)` + 序号守卫）：新增 `pools` ref、`poolsLoading`、`poolsLoadError`，Zone 切换时调既有 `listPools({ zone_id: zoneId, limit: 100 })`（pools.ts:10-14 已支持 zone_id 过滤，无需新方法）加载；`createForm.pool_id` 为 `number | undefined`，未选时提交 payload 不携带 `pool_id`（契约可选字段，client 序列化时空值省略）。**Zone 切换必须同步重置 `pool_id`**（与 image_id 同模式，vms.vue:291）：否则用户先选 Zone A 的池再切 Zone B，残留 pool_id 提交后撞 404。池列表为空时下拉处展示提示 + 链接到 `/ip-pools` 页。

### 6. 契约与前端 client 同步

`docs/openapi.yaml`（权威源）：`POST /vms` requestBody 增加可选 `pool_id`（integer），400 错误分支补充 `no_available_ip_pool`，`node_unavailable` 描述收窄为"有候选节点但探测失败"，**ErrorDetail enum（openapi.yaml:1449-1469）同步加码**。同步 `cp docs/openapi.yaml api/swagger/openapi.yaml`。`docs/api-errors.md` 新增一行错误码（HTTP 400，语义"区域无可用 IP 池"）——该文档第 34 行要求新增错误码伴随 API 版本升级，本项目无版本化 API 机制、既有新增错误码（如 invalid_vm_id、user_has_resources）均直接登记，本次按同例登记并在 PR 说明标注。前端 `npm run api:check` 校验生成的 client 与契约一致（不落仓库）；`web/app/api/contract-verify.ts:51-54` 的 `_AssertCreateVMReq`/`_AssertCreateVMReqRequired` 严格断言必须同步新增 pool_id（否则 typecheck 失败）。

## Risks / Trade-offs

- **错误码新增属契约变更**：新错误码 `no_available_ip_pool` 会出现在 OpenAPI 契约，属非破坏性新增（不影响既有客户端），但 api-errors.md 将"新增错误码"定义为破坏性变更 → 双副本同步 + api-errors 登记 + PR 说明标注
- **`selectPoolAndNode` 签名扩展波及测试**：`TestSelectPoolAndNodeSkipsUnreachablePools` 第三段（vm_service_test.go:1014-1018，无池 → KindNodeUnavailable）断言**语义反转**为 no_available_ip_pool；其余既有用例（镜像/不可达场景均已配池）断言保持 → 补齐：指定池成功 / 指定池非本区域 404 / 指定池白名单空 / 无池兜底
- **image_not_available_in_zone 触发条件收窄**：无池+无镜像/无启用节点场景从 image_not_available 变为 no_available_ip_pool，spec 与 api-errors.md 的触发场景描述须同步修订，避免 spec 与行为漂移 → api-errors.md:56 行描述改写
- **前端联动竞态与残留**：Zone 快速切换时旧池列表覆盖新 Zone → 沿用镜像的序号守卫模式（imagesSeq 同款 poolsSeq）；Zone 切换残留 pool_id 会撞 404 → watch 中同步重置
- **contract-verify.ts 严格断言**：`_AssertCreateVMReq`/`_AssertCreateVMReqRequired` 断言 CreateVMRequest 的 keyof/RequiredKeys，新增 pool_id 后 typecheck 编译失败 → tasks 4.x 显式列出
- **e2e 断言**：既有 e2e 无创建 VM 池相关断言（镜像场景已配池），不受影响；新增 e2e 用例：区域无池创建 VM → 400 no_available_ip_pool
