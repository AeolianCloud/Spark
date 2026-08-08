## 1. 后端：错误码与调度重构

- [x] 1.1 新增 `KindNoAvailableIPPool ErrorKind = 110` 与构造器 `noAvailableIPPoolf`（service/vm_service.go，与 imageNotAvailablef 同域），消息模板区分「区域无 IP 池」与「池白名单无候选节点」两种子因
- [x] 1.2 重构 `selectPoolAndNode`（service/vm_service.go:565）：签名增加 `poolID *int64`；指定池时只遍历该池（GetPoolNodes + poolCandidates），池候选为空直接返回 `no_available_ip_pool`；缺省时保持现有全池遍历
- [x] 1.3 调整失败分支优先级（vm_service.go:611-620）：① 无池/全池无候选（无论镜像状态）→ no_available_ip_pool；② 有池候选但无镜像且无扫描失败 → image_not_available_in_zone；③ 扫描失败/探测失败 → node_unavailable（语义不变）
- [x] 1.4 `CreateVM` 请求体（CreateVMRequest）新增可选 `pool_id` 字段；校验：`pool_id` 非空时须 `> 0`（否则 bad_request，仿 zone_id 模式）且 GetPool 后 `pool.ZoneID == req.ZoneID`（否则 404）；通过后传入 selectPoolAndNode
- [x] 1.5 `api/handlers/vm_handler.go`：新增 `CodeNoAvailableIPPool = "no_available_ip_pool"` 常量，`mapVMServiceError` 增加 `case KindNoAvailableIPPool → NewError(400, CodeNoAvailableIPPool, ...)`（对照 KindImageNotAvailable 的接线模式）

## 2. 后端：测试

- [x] 2.1 新增单测：区域无池 → no_available_ip_pool（message 含区域；覆盖"无池+有镜像"与"无池+无镜像"两种子场景）
- [x] 2.2 新增单测：池白名单为空 / 白名单∩启用节点为空 → no_available_ip_pool
- [x] 2.3 新增单测：指定 pool_id 成功（仅该池调度）；指定 pool_id 不存在 / 不属于该区域 → 404；pool_id 传 0 / 负数 → bad_request
- [x] 2.4 更新既有用例 `TestSelectPoolAndNodeSkipsUnreachablePools` 第三段（vm_service_test.go:1014-1018）：无池断言语义反转为 KindNoAvailableIPPool；其余既有用例（镜像/不可达场景已配池）断言保持
- [x] 2.5 新增 e2e 用例：区域无池创建 VM → 400 no_available_ip_pool（e2e/e2e_test.go；既有 e2e 断言已核对不受影响）

## 3. 契约同步

- [x] 3.1 docs/openapi.yaml：POST /vms requestBody 增加可选 pool_id（integer）；400 错误分支补充 no_available_ip_pool；node_unavailable 描述收窄为"有候选节点但探测失败"；ErrorDetail enum（约 1449-1469 行）同步加码；image_not_available_in_zone 触发场景描述收窄
- [x] 3.2 cp docs/openapi.yaml api/swagger/openapi.yaml 保证双副本字节一致
- [x] 3.3 docs/api-errors.md 新增 no_available_ip_pool（400，区域无可用 IP 池）并注明触发场景；修订 image_not_available_in_zone 触发场景描述（收窄为"有池候选但无镜像"）；按"新增错误码"规则登记并注明无版本化机制同例处理
- [x] 3.4 npx --yes @redocly/cli lint docs/openapi.yaml 通过

## 4. 前端

- [x] 4.1 web/app/pages/vms.vue：createForm 增加 `pool_id`；Zone 切换 watch 中同步重置 `pool_id`（与 image_id 同模式）并联动加载该 Zone 池列表（复用既有 `listPools({ zone_id, limit })`，pools.ts:10-14；新增 poolsSeq 序号守卫）
- [x] 4.2 池列表为空时下拉处展示「该区域未配置 IP 池」提示并链接到 /ip-pools；加载失败展示错误并允许重试
- [x] 4.3 提交 payload 构造处：pool_id 为空时从 payload 中省略（不传 null）；validateCreateForm 不强制要求 pool_id（可选）
- [x] 4.4 web/app/api/contract-verify.ts：`_AssertCreateVMReq`（keyof 9→10）与 `_AssertCreateVMReqRequired`（RequiredKeys 不变，pool_id 为可选）断言同步更新，保证 `npm run typecheck` 通过
- [x] 4.5 npm run api:check 通过（生成 client 与契约 git diff 为空）

## 5. 收尾

- [x] 5.1 go test -count=1 ./... 全绿；go vet ./...
- [x] 5.2 reviewer 审查后端改动（含 Kind 接线与错误分支重构）；安全相关（无新增敏感面，常规审查即可）
- [x] 5.3 按 PR checklist 核对：契约双副本一致、redocly lint 通过、api-errors 已更新、前端 typecheck 通过
