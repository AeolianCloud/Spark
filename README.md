# spark

基于 Proxmox VE 的虚拟化管理后端：对接 PVE 标准 JSON API，提供云主机（VM）的全生命周期管理 REST 接口。

## 技术栈

- **Go 1.26.5+**（`go.mod` 的 `go` 指令要求；配合 `GOTOOLCHAIN=auto` 会自动下载匹配工具链）
- **gin**（HTTP 框架）+ **pgx/v5**（PostgreSQL 驱动）
- **PostgreSQL 17+**（表结构与迁移由内嵌 SQL 管理，启动时自动执行）
- **Proxmox VE 7/8/9 节点**（通过 `https://{host}:8006/api2/json` 交互）

## 快速开始

### 环境要求

| 组件 | 要求 |
| --- | --- |
| Go | 1.26.5+ |
| PostgreSQL | 17+（角色/库由部署者准备，服务只负责建表） |
| PVE 节点 | 7 / 8 / 9，开启 API（默认 8006 端口），有 API token |

### 配置

配置按「默认值 → `config/config.yaml` → `SPARK_*` 环境变量」的优先级合并：

| 配置项 | 环境变量 | 说明 |
| --- | --- | --- |
| `server.port` | `SPARK_SERVER_PORT` | 监听端口，默认 8080 |
| `database.dsn` | `SPARK_DATABASE_DSN` | PostgreSQL 连接串，如 `postgres://spark:xxx@localhost:5432/spark?sslmode=disable` |
| `crypto.encryption_key` | `SPARK_CRYPTO_ENCRYPTION_KEY` | base64 编码的 32 字节 AES 密钥，用于加密 VM 的 cloud-init 密码 |
| `log.level` | `SPARK_LOG_LEVEL` | `debug` / `info` / `warn` / `error` |
| `images.download_host_allowlist` | `SPARK_IMAGES_DOWNLOAD_HOST_ALLOWLIST` | 镜像下载源域名白名单（逗号分隔），默认内置 5 个云镜像源；空列表拒绝所有下载 |

生成加密密钥：

```bash
openssl rand -base64 32
```

> 生产环境必须替换 `config/config.yaml` 中的示例密钥（启动时检测到示例值会打印警告）。
> 本地敏感配置（真实数据库密码、加密密钥）可放入仓库根目录的 `.env.local`（已 gitignore），
> 通过 `set -a; source .env.local; set +a; go run ./cmd/server` 加载，`config/config.yaml`
> 只保留占位示例值。

### 构建与运行

```bash
go build ./cmd/server && ./server        # 构建 + 启动
go run ./cmd/server                      # 直接运行
```

启动流程：加载配置 → 连接 PostgreSQL（连接池 + Ping 健康检查）→ 自动执行内嵌迁移（`database.Migrate`，幂等，按版本号记录在 `schema_migrations`）→ 启动 HTTP 服务。

Web 管理界面（`web/`）的生产部署（nginx 静态托管 + `/api` 反代、无鉴权网络隔离提示）见 [docs/web-deployment.md](docs/web-deployment.md)。

## PVE 节点准备

每个被管理的 PVE 节点需要：

1. **确认网桥**：`vmbr0` 存在且可路由（创建 VM 时网络固定挂 `bridge=vmbr0`）。

2. **登记 cloud 镜像**（qcow2 格式）：通过 API 登记镜像下载地址（`POST /images`），镜像由目标 PVE 节点代发下载到 local 存储的 import 目录（`/var/lib/vz/import/`）：

   ```json
   {"name": "debian-12-cloud", "default_user": "debian",
    "download_url": "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2"}
   ```

   随后用 `POST /images/:id/download` 把镜像下载到目标节点（`node_ids` 或 `zone_id` 指定）；也可以手工 `scp` 同名文件到节点的 `/var/lib/vz/import/`，两种方式都会被 PVE 扫描识别。创建 VM 时该镜像会以 `import-from` 的形式交给 PVE 在 `qmcreate` 任务中一步完成导入。

3. **创建 API token**（在 PVE 节点 shell 执行，或 Web 界面 DataCenter → Permissions）：

   ```bash
   pveum user add spark@pve
   pveum acl modify / -user spark@pve -role PVEVMAdmin
   pveum user token add spark@pve spark --privsep 0
   # 输出中的 secret 即为 api_token，形如 spark=xxxxxxxx-xxxx-...
   ```

   在 `POST /zones/:id/nodes` 中登记：`api_user` 填 `spark@pve`，`api_token` 填完整 secret；`host` 填节点 IP 或主机名。

4. **确认存储类型**：`local-lvm` / `local` 等 PVE 存储（`pvesm status` 查看），通过 `POST /storage-types` 登记抽象名与 `pve_storage` 映射。

## API 概览

错误统一返回 `{"error": {"code", "message"}}` 并携带 `x-ms-error-code` 响应头；`code` 取值：`bad_request` / `not_found` / `method_not_allowed` / `conflict` / `unprocessable_entity` / `node_unavailable` / `ip_exhausted` / `vm_not_ready` / `disk_shrink_not_allowed` / `image_not_available_in_zone` / `internal_error`（全量清单与触发场景见 [docs/api-errors.md](docs/api-errors.md)）。完整机器可读契约（OpenAPI 3.0）见 [docs/openapi.yaml](docs/openapi.yaml)。

列表端点统一分页：`limit` 默认 25、上限 100（超出截断）、`offset` 默认 0，非法值返回 400；响应头 `X-Total-Count` 携带过滤后（分页前）的总条数。

| 方法 | 路径 | 说明 | 主要参数 |
| --- | --- | --- | --- |
| GET | `/healthz` | 健康检查（含 DB 连通性） | — |
| GET | `/docs` | Swagger UI 在线文档（swagger-ui 静态资源按 `/docs/*` 提供） | — |
| GET | `/openapi.yaml` | OpenAPI 契约原文（`application/yaml`） | — |
| POST | `/zones` | 创建区域 | `name` |
| GET | `/zones` | 区域列表（含节点；分页 + `X-Total-Count`） | `limit?`、`offset?` |
| POST | `/zones/:zone_id/nodes` | 登记 PVE 节点 | `name`、`host`、`api_user`、`api_token`、`enabled?` |
| GET | `/zones/:zone_id/nodes` | 区域节点列表 | — |
| PUT | `/nodes/:id` | 更新节点（空 `api_token` 保留原密钥） | 同上 |
| POST | `/ip-pools` | 创建 IP 池（自动展开为逐地址记录） | `zone_id`、`name`、`network_cidr`、`gateway`、`dns` |
| GET | `/ip-pools` | IP 池列表（可按 `zone_id` 过滤；分页） | `zone_id?`、`limit?`、`offset?` |
| PUT | `/ip-pools/:id/nodes` | 勾选该池可用的节点白名单 | `node_ids` |
| GET | `/ip-pools/:id/nodes` | 池的节点白名单 | — |
| POST | `/storage-types` | 登记存储类型 | `name`、`display_name`、`pve_storage` |
| GET | `/storage-types` | 存储类型列表（分页） | `limit?`、`offset?` |
| GET/PUT/DELETE | `/storage-types/:id` | 存储类型查/改/删（DELETE → 204） | — |
| POST | `/images` | 登记 cloud 镜像 | `name`、`default_user`、`download_url`（http(s)，必填） |
| GET | `/images` | 镜像列表；`?zone_id=` 返回区域内至少一个启用节点存在该镜像的条目（含各节点存在状态；分页） | `zone_id?`、`limit?`、`offset?` |
| GET | `/images/:id` | 镜像详情 | — |
| GET | `/images/:id/nodes-status` | 镜像在各启用节点上的存在状态（`?zone_id=` 限定区域） | `zone_id?` |
| POST | `/images/:id/download` | 受理镜像下载到目标节点/区域（202 异步，`Location` 指向 `GET /images/:id/operations`） | `node_ids` 或 `zone_id` |
| GET | `/images/:id/operations` | 镜像下载操作历史（分页） | `limit?`、`offset?` |
| POST | `/vms` | 创建 VM：分配 IP → 落库 → 异步 PVE 创建链，立即返回 201 | `name`、`cpu`、`mem_mb`、`disk_gb`、`image_id`、`storage_type_id`、`zone_id`、`password` |
| GET | `/vms` | 列表（穿透式合并各节点实时状态，节点故障出现在 `warnings`；分页） | `limit?`、`offset?` |
| GET | `/vms/:id` | 详情（穿透实时状态） | — |
| POST | `/vms/:id/start\|stop\|restart` | 生命周期操作（202 异步派发，`Location` 指向 `GET /vms/:id`） | — |
| PATCH | `/vms/:id` | 升降配；缺失/null 字段保留现值，磁盘只增、缩小返回 422；返回完整 VM 穿透状态 | `cpu?`、`mem_mb?`、`disk_gb?` |
| DELETE | `/vms/:id` | 销毁：PVE 侧删除（含 purge）→ 释放 IP → 删本地记录；204 同步返回，重复销毁幂等（404） | — |

## 业务模型

- **区域（zone）**：部署分区，聚合节点、IP 池与 VM。
- **节点（pve_nodes）**：PVE 节点登记（host + API token），可启停（`enabled`）。
- **IP 池（ip_pools / ips）**：池登记网段/网关/DNS，展开为逐地址记录；节点白名单（`ip_pool_nodes`）限定哪些节点可用该池。分配采用「随机选 + 条件 UPDATE 原子占位」，并发安全。
- **存储抽象（storage_types）**：`name/display_name` 对外，`pve_storage` 映射真实 PVE 存储。
- **镜像目录（images）**：登记镜像元数据（`name`/`default_user`/`download_url`）；镜像在各节点的存在状态由 PVE 实时扫描（local 存储 import content，即 `/var/lib/vz/import/`）；区域可用镜像 = 区域内至少一个启用节点存在该镜像。
- **VM 状态语义**：DB 不存状态镜像（穿透式）。创建返回 `creating`；异步供给链失败后详情/列表返回 `failed`（`provision_error` 携带原因）；供给成功后的状态实时读取自 PVE（`running`/`stopped` 等）。

## 契约与工具链

API 契约以 [docs/openapi.yaml](docs/openapi.yaml)（OpenAPI 3.0）为唯一源，错误码全量清单见 [docs/api-errors.md](docs/api-errors.md)。

- **在线浏览**：服务启动后访问 `GET /docs`（Swagger UI 页面，swagger-ui 静态资源按 `/docs/*` 提供）；`GET /openapi.yaml` 直接输出契约原文。两条路由刻意不写入契约本身的 `paths`，避免契约自指。
- **契约校验**（redocly，豁免规则见 `.redocly.lint-ignore.yaml`）：

  ```bash
  npx --yes @redocly/cli lint docs/openapi.yaml
  ```

- **类型生成**（openapi-typescript，生成物不落仓库）：

  ```bash
  npx --yes openapi-typescript docs/openapi.yaml -o /tmp/spark-openapi-types.ts
  ```

- **Embed 副本同步**：`GET /openapi.yaml` 输出的是 `api/swagger/openapi.yaml`（go:embed 内嵌副本，与 docs/openapi.yaml 字节一致；`go:embed` 无法跨包读取 `docs/`）。修改契约后需同步复制，保证两份一致：

  ```bash
  cp docs/openapi.yaml api/swagger/openapi.yaml
  ```

## 测试

```bash
# 1. 常规单元测试（pgxmock 等，无外部依赖）
go test -count=1 ./...

# 2. 真实 PostgreSQL 集成测试（IP 并发分配，-tags=pg）
SPARK_TEST_DSN='postgres://spark:spark@127.0.0.1:5432/spark_test' \
  go test -tags=pg ./repository/ -count=1 -run TestPG -v

# 3. 端到端测试（真实 DB + 内存假 PVE 服务器，走完整 HTTP 栈，-tags=e2e）
SPARK_E2E_DSN='postgres://spark:spark@127.0.0.1:5432/spark_test' \
  go test -tags=e2e ./e2e/ -count=1 -v
```

- `-tags=pg` 用例会执行 `DELETE` 清理 `ips` / `ip_pool_nodes` / `ip_pools` 表；
- `-tags=e2e` 用例会 `TRUNCATE` 全部业务表（zones、pve_nodes、ip_pools、ips、storage_types、images、vms），测试前后各执行一次，可与 pg 测试共用同一数据库。
- 假 PVE 服务器通过 `api.WithVMClientFactory` 注入（VM 服务探活/供给/生命周期全部走注入的客户端）；节点以 `127.0.0.1:<随机端口>` 登记，客户端经 `pve.WithPort` 真实连接假服务器的监听端口。

## 已知限制（v1）

- **无认证**：API 完全开放，前端接入时再定鉴权方案。
- **无任务持久化/重试**：PVE 任务只在供给链内同步轮询，不落库；失败不自动重试。
- **异步失败仅标记**：供给失败只写 `provision_error`，IP 不自动释放（避免复用脏 IP，由运营手工回收）；PVE 侧残留的半成品 VM 需手工清理。
- **无计费/配额**：不限制 CPU/内存/磁盘用量。
- 磁盘不支持缩容、迁移、克隆、快照等扩展操作。
