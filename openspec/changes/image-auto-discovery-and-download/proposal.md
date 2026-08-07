## Why

镜像登记完全依赖手工维护 `node_images`（节点名 -> 路径的 DB 快照）：PVE 侧镜像的实际存在情况变化（上传/删除）Spark 无从感知，也无法把镜像自动分发到节点，管理员需要 SSH 到每台节点手动下载云镜像。创建 VM 时镜像校验与节点选择脱节——节点挑选只看可达性，镜像路径缺失要到 provision 阶段才暴露，导致创建后失败、只能事后补救。

## What Changes

- **BREAKING**：`images` 表删除 `node_images` 列（节点存在状态不再落库），新增必填的 `download_url` 列——镜像登记变为 `name + default_user + download_url`（前端固定下载地址，如 debian 云镜像 latest 的完整 URL）
- 节点上的镜像存在状态以 **PVE 为权威源**：通过 `GET /nodes/{node}/storage/local/content?content=import` 实时扫描 `/var/lib/vz/import/`，文件名与 `download_url` 尾部文件名匹配判定"已下载"，按节点展示状态
- 新增按节点镜像下载：单节点或批量（节点 id 列表 / 区域全部启用节点），固定下载到 `local` 存储的 import 目录，走 PVE `download-url` 异步任务，落库 `image_operations` 表追踪结果（沿用 `vm_operations` 模式）
- 创建 VM 的镜像校验语义变化（spec 级）：`ListImagesByZone` 从"区域所有启用节点都有镜像才可用"放宽为"区域内至少一个启用节点有镜像即可用"；节点选择逻辑（`selectPoolAndNode`）加入镜像过滤——候选节点 = 可达 && 该节点 import 目录存在该镜像，全区域无节点有该镜像时返回 `image_not_available_in_zone`
- 手工上传到节点 import 目录的镜像文件无需登记即可被扫描识别为"已下载"（文件名匹配）

## Capabilities

### New Capabilities

（无——镜像下载与状态扫描均属 image-catalog 范畴，无需新建 capability）

### Modified Capabilities

- `image-catalog`: 镜像登记模型（node_images 快照 -> download_url + PVE 实时扫描）、按节点可用性汇总查询的"交集"语义放宽为"存在性"、新增按节点下载与下载任务追踪行为

## Impact

- `database/migration/0009_*.sql`：删除 `images.node_images`，新增 `images.download_url`、`image_operations` 表
- `model/entities.go`：`Image` 结构调整，新增 `ImageOperation`；VM 创建流程中 `image.NodeImages[nodeName]` 路径来源被替换
- `repository/image_repo.go`：CRUD 改造 + `image_operations` 仓库
- `service/image_service.go`、`vm_service.go`：下载编排、镜像感知的节点选择（`selectPoolAndNode`）、创建校验重写
- `pve/client.go`：新增 storage content 枚举与 `download-url` 调用方法（`content=import`）
- `api/handlers/image_handler.go` + `api/router.go`：镜像列表/登记请求体变化，新增下载端点
- `e2e/`：fake PVE 服务器补充 storage content 与 download-url 端点，保证 e2e 可用
- `docs/openapi.yaml` 与 `api/swagger/openapi.yaml` 双副本同步，operationId 完整
- `docs/api-errors.md`：错误码变化同步
