## 1. 数据库迁移

- [x] 1.1 新增 migration 0011：storage_types 加 zone_id（PL/pgSQL：单 zone 自动归入，多 zone 报错中止）、enabled/type/content 列、name DROP NOT NULL、DROP COLUMN display_name、(zone_id, pve_storage) 唯一索引
- [x] 1.2 migration 测试：覆盖单 zone 归入、多 zone 中止、空表加约束三路径（database/migrate_test.go 内容回归 + database/migrate_pg_test.go 真实执行，-tags=pg）
- [x] 1.3 model.StorageType 更新（zone_id/enabled/type/content，去 display_name，name 指针化或加 Valid 标记）

## 2. PVE 客户端

- [x] 2.1 pve/storage.go 新增 ListStorage（GET /storage）与 PVEStorage 建模（storage/type/content/shared/nodes），content 空值策略与现有接口一致
- [x] 2.2 pve/storage_test.go 覆盖正常解析与 PVE 错误封装（UpstreamError）

## 3. Repository 与 Service

- [x] 3.1 StorageTypeRepository 重写：UpsertByZonePveStorage（仅覆盖 type/content）、UpdateMeta（name/enabled）、ListPage 支持 zone_id 过滤、Get/Delete 保留、Count 支持 zone 过滤
- [x] 3.2 repo pg 测试同步（storage_type_repo_pg_test.go 按新列与语义重写）
- [x] 3.3 StorageTypeService 新增 SyncZone：选 zone 首个可达启用节点 → ListStorage → 事务内同步（created/updated/deleted/skipped 摘要，FK 跳过）
- [x] 3.4 StorageTypeService 重写 Create/Update：Create 移除，Update 仅 name/enabled，验证逻辑更新（name 可空）
- [x] 3.5 service 单测：同步语义（新/更新/删除/FK 跳过/部分失败不落库）、Update 语义、摘要计算

## 4. VM 创建校验

- [x] 4.1 CreateVM 追加两道闸：storageRepo.Get 后校验 enabled 与 content 含 images，bad_request 错误
- [x] 4.2 vm_service 单测覆盖两个拒绝场景（现有 fakeVMStorageTypeRepository 扩充 enabled/content 字段）

## 5. 扫描触发

- [x] 5.1 节点注册成功分支调用 SyncZone（失败仅记日志不阻断注册）
- [x] 5.2 node_service 单测：注册成功后触发扫描、扫描失败不影响注册结果

## 6. API 层

- [x] 6.1 handler 重写：移除 POST，PUT 仅 name/enabled，新增 POST /storage-types/scan?zone_id= 返回摘要，GET 支持 zone_id 过滤，响应含 capabilities 派生对象
- [x] 6.2 router 挂载与错误映射核对（新错误码走现有错误体系）
- [x] 6.3 docs/openapi.yaml 与 api/swagger/openapi.yaml 双副本同步（含 capabilities 结构、scan 端点、移除 POST），redocly lint 通过
- [x] 6.4 docs/api-errors.md 核对新增错误语义（storage type disabled / cannot store disks）

## 7. 前端

- [x] 7.1 web/app/api/types.ts、client.ts 按新契约重生成，npm run api:check 零 diff
- [x] 7.2 pages/storage-types.vue 改"扫描按钮 + 摘要 + name/enabled 编辑"形态，去 display_name 表单
- [x] 7.3 pages/vms.vue 存储下拉标签改 name || pve_storage

## 8. 端到端与收尾

- [x] 8.1 e2e fake PVE 服务器新增 /storage 应答与扫描场景（e2e/ 现有 fake 机制）
- [x] 8.2 全量测试跑通（go test、pg 测试、e2e 标签）
- [x] 8.3 CONTEXT.md 更新 StorageType 术语（扫描同步、zone 归属、enabled、能力体现），README 存储章节同步
- [x] 8.4 reviewer 审查（核心路径）+ 涉及认证/输入校验处 security-reviewer 审查（两轮：初查 9.1-9.5 + 复审）

## 9. 审查问题修复（reviewer/security-reviewer 反馈）

- [x] 9.1 P1 跨 zone 存储校验：CreateVM 两道闸后追加 `storageType.ZoneID != req.ZoneID → not_found`（语义与 ip pool 的 zone 归属一致），补单测（vm_service_test.go）
- [x] 9.2 P2 权限粒度：初版整组 requireAdmin 被第三轮复审判为 blocker B1（GET 读列表被锁死将阻断 user 创建 VM 的必填参考数据），修正为读操作（GET 列表/详情）仅 requireAuth、写操作（scan/PUT/DELETE）挂 requireAdmin；openapi 双副本读操作移除 403、写操作保留，e2e 与 handler 测试改 user GET 200 / 写操作 403 断言，api-errors 措辞收窄
- [x] 9.3 P3 SyncZone 开头校验 zone 存在（注入 StorageTypeZoneRepository 只读接口，构造签名变更同步 router 装配与测试），zone 不存在返回 not_found，补单测
- [x] 9.4 P4 Update 的 name trim 后超过 255 字符（rune）返回 bad_request，openapi 双副本 StorageTypeUpdateRequest.name 补 maxLength: 255，补单测
- [x] 9.5 P5 测试补齐：storage_type_handler_test.go 新建（capabilities 派生纯函数、Scan 400/200/503/404、Update 置空 name、requireAdmin 分层 401/403）；SyncZone 补 pgx.ErrNoRows 删除路径用例；SyncZone 方法注释补并发语义（逐条幂等收敛、摘要计数可能失真、数据一致）

## 10. 节点挂载快照与调度联动（扩展）

- [x] 10.1 migration 0012：storage_types 加 nodes TEXT NOT NULL DEFAULT ''；pg 测试补 0012 路径断言
- [x] 10.2 model.StorageType 加 Nodes []string（json "nodes"）；repo UpsertByZonePveStorage 加 nodes 参数、列清单更新
- [x] 10.3 SyncZone 快照 nodes（PVEStorage.Nodes join 逗号串落库）；repo pg 测试补 nodes 覆盖与保留语义
- [x] 10.4 handler DTO 响应加 nodes []string（split）；openapi 双副本 StorageType 加 nodes（nullable=false，description 注明空数组=不限制）并同步前端生成物
- [x] 10.5 CreateVM 调度联动：poolCandidates 后、镜像扫描前插入存储挂载过滤（nodes 非空时排除未挂载节点）；无候选 → storage_not_available_in_zone（bad_request，api-errors.md 同步）
- [x] 10.6 vm_service 单测：存储挂载过滤（命中/全排除/空 nodes 放行）、storage_not_available_in_zone 错误、与镜像/池候选组合场景
- [x] 10.7 前端管理页按节点分组：storage-types.vue 按 nodes（+空=所有节点）分组展示；创建 VM 存储下拉补挂载节点提示（10.7a 契约生成物同步：api:gen 重生成 schema.d.ts、contract-verify.ts 补 _AssertStorageTypeNodes，api:check 零 diff；10.7b 管理页按节点分组；10.7c vms.vue 存储下拉 description 展示挂载节点）
- [x] 10.8 e2e：fake /storage 应答带 nodes 字段，扫描后快照正确；VM 创建调度按挂载过滤与拒绝场景
- [x] 10.9 全量验证：go test（含 -tags=pg、-tags=e2e）、openapi 双副本、redocly lint、前端 api:check/typecheck/build（后端部分已完成；前端部分归 10.7，前端 agent 负责）
- [x] 10.10 reviewer 复审（核心路径）+ security-reviewer（调度过滤与错误面）——发现 H1 高危（见第 11 组）与若干 minor，均已修复

## 11. 审查问题修复（第 10 组扩展的安全/UX 反馈）

- [x] 11.1 H1 权限修复（high）：节点注册/更新（POST /zones/:zone_id/nodes、PUT /nodes/:id）挂 requireAdmin——阻断"user 注册伪造节点 → 自动扫描注入存储快照"攻击链；openapi 双副本补 403/security、api-errors.md 同步、handler 测试补 403 断言
- [x] 11.2 前端分组兜底：storage-types.vue 将 nodes 非空但与 zone 节点无交集的存储归入"所有节点"组（修复数据消失缺陷）；补 ghost 节点场景说明
- [x] 11.3 design.md Risks 修正：nodes 快照过期分两个方向表述（挂载新增方向保守；摘挂方向不保守——调度到已摘挂节点时异步供给失败，重扫自愈）
- [x] 11.4 e2e ghost 场景补"不落库、不占 IP"断言（GET /vms 无该行 + 池空闲 IP 数不变）
- [x] 11.5 L2 可观测性：扫描/调度侧对"快照节点名在 zone 启用节点中无匹配"记 warning 日志（便于定位 PveName 未回填问题）
- [x] 11.6 全量验证 + reviewer 复审修复
- [x] 11.7 前端分组默认态：全部区域视图也按节点分组（组标题带区域前缀如 "zoneA / pve1"，归属校验 s.zone_id === zone.id 防跨 zone 同名节点混淆；nodes 空归"所有节点"组）
