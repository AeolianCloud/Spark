## 1. 数据库迁移

- [x] 1.1 编写 `database/migration/0009_image_download.sql`：`images` 表删除 `node_images` 列、新增非空 `download_url TEXT` 列；新建 `image_operations` 表（image_id、node_id、action、result、error_message、upid、created_at、updated_at），外键与索引
- [x] 1.2 迁移测试与既有迁移对齐（`database/migrate_test.go` 模式），全量迁移可重复执行

## 2. model 层

- [x] 2.1 `model.Image` 删除 `NodeImages` 字段，新增 `DownloadURL`；新增 `model.ImageOperation`（含 action/result 常量，沿用 vm_operations 取值风格）
- [x] 2.2 更新 `model` 相关注释与领域术语（CONTEXT.md 的 Image 词条从"NodeImages 记录路径"改为"download_url + PVE 实时扫描"）

## 3. PVE 客户端

- [x] 3.1 `pve` 包新增 `ListStorageContent(ctx, node, storage, content string)`（`GET /nodes/{node}/storage/{storage}/content`），定义 `StorageContent{VolID, Name}` 结构
- [x] 3.2 `pve` 包新增 `DownloadURL(ctx, node, storage, content, filename, url string)`（`POST /nodes/{node}/storage/{storage}/download-url`），复用 `decodeUPID` 解析返回的 UPID
- [x] 3.3 两个新方法补齐单测（沿用 `client_test.go` 的 httptest 模式）与中文注释

## 4. repository 层

- [x] 4.1 `image_repo.go`：Create/Get/GetByName/List/ListPage 去除 node_images 列、加入 download_url 列；Create 校验 download_url 非空
- [x] 4.2 新建 `repository/image_operation_repo.go`：CreateOperation（result='running' 插入）、UpdateOperationResult（success/failed + error_message + updated_at）、ListOperationsByImage（分页 + 总数）
- [x] 4.3 repository 单测补齐（mock 模式与 `image_repo_test.go` 对齐）

## 5. service 层

- [x] 5.1 `ImageService` 注入节点仓库与 PVE 客户端工厂（复用 `VMService.SetClientFactory` 模式），构造签名与 `router.go` 装配点同步更新
- [x] 5.2 实现节点镜像存在性扫描：并行调用各节点 `ListStorageContent`，文件名匹配 download_url 尾部（path.Base），返回 `[{node_id, node_name, pve_name, downloaded}]`
- [x] 5.3 `ListImagesByZone` 语义改为"区域内至少一个启用节点存在该镜像"，返回结构携带各启用节点存在状态（D7）；同步调整 `slicePage`/过滤逻辑
- [x] 5.4 实现下载编排（D6）：单节点/多节点/区域全部启用节点解析、每节点独立 goroutine（background ctx + 超时 + panic recover）、UPID 轮询（复用 WaitTask）、结果落库
- [x] 5.5 实现 `GetImageNodeStatus`（单镜像节点状态）与 `ListImageOperations`（下载历史分页）
- [x] 5.6 `VMService.CreateVM`：删除 node_images 交集前置校验（vm_service.go:318-326）；`selectPoolAndNode` 增加镜像参数，候选节点先按扫描结果过滤镜像存在性再测可达性；无候选返回 `KindImageNotAvailable`
- [x] 5.7 `provisionVM` 改用 volid 拼 `DiskImportString`（D4），`imagePath`/`NodeImages` 引用清除；provision 期间失败路径保持 failProvision 语义
- [x] 5.8 服务层单测补齐：扫描匹配、存在性过滤、下载编排（fake 仓库 + fake 客户端）、镜像感知调度（a 节点有/b 节点无 → 调度到 a）

## 6. API 层

- [x] 6.1 `image_handler.go`：`POST /images` 请求体改 `{name, default_user, download_url}`；`GET /images?zone_id=` 响应携带 node_statuses；新增 `GET /images/{id}/nodes-status`、`POST /images/{id}/download`（202 + operations）、`GET /images/{id}/operations`
- [x] 6.2 `router.go` 装配新路由与 ImageService 新依赖；`GET /images/{id}/nodes-status` 与 operations 分页复用 `parsePagination`
- [x] 6.3 handler 层测试补齐（请求体校验、错误映射、分页、202 语义）

## 7. OpenAPI 契约与文档

- [x] 7.1 `docs/openapi.yaml`（权威源）同步：images 登记请求体、列表响应结构、三个新端点及 operationId、错误码（镜像不可用语义变更说明）
- [x] 7.2 同步 `api/swagger/openapi.yaml` 双副本字节一致；`npx --yes @redocly/cli lint docs/openapi.yaml` 通过
- [x] 7.3 `docs/api-errors.md` 同步错误码变化（若有新增/变更）

## 8. e2e 与收尾

- [x] 8.1 fake PVE 服务器补齐 storage content 与 download-url 端点（可预置文件清单、异步任务模拟），e2e 用例覆盖：镜像列表带节点状态、下载后状态翻转、创建 VM 调度到有镜像节点
- [x] 8.2 `go test ./...`、`go test -tags=e2e ./e2e/ -count=1 -v` 全量通过
- [x] 8.3 按 review 分级走 `reviewer` 审查（涉及 API/敏感路径按需 `security-reviewer`）
