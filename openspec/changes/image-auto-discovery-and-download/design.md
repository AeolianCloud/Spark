## Context

现状与动机见 `proposal.md`。相关现状：

- `images` 表：`id, name, default_user, node_images(JSONB), created_at`，node_images 手工维护（节点 pve_name -> 存储路径）
- `ImageService` 仅依赖 `ImageRepository`（无节点仓库、无 PVE 客户端）
- `VMService.CreateVM` 前置校验（`vm_service.go:318-326`）用 node_images 交集判断镜像在区域所有启用节点可用；`selectPoolAndNode`（`vm_service.go:415`）只按"可达"挑节点；`provisionVM`（`vm_service.go:466`）从 `image.NodeImages[nodeName(node)]` 取路径拼 `import-from`（`pve.DiskImportString`）
- PVE 提供标准 API：`GET /nodes/{node}/storage/{storage}/content?content=import` 枚举 `/var/lib/vz/import/`（dir 存储）下的文件；`POST /nodes/{node}/storage/{storage}/download-url` 异步下载（返回 UPID）；`pve/task.go` 已有 WaitTask 轮询能力
- `vm_operations` 表已建立异步操作审计模式（action/result/error_message）

## Goals / Non-Goals

**Goals:**
- 镜像登记去掉 node_images，节点存在状态以 PVE 为权威源实时获取
- 按节点下载镜像（单节点/多节点/区域批量），下载结果可审计
- 创建 VM 的节点选择感知镜像存在性，杜绝"创建后 provision 失败"的镜像路径问题

**Non-Goals:**
- 不做下载进度条实时推送（轮询即足够，image_operations 落库供查询）
- 不支持自定义下载存储（固定 local）
- 不做 latest 目录解析（download_url 为完整固定 URL）
- 不处理迁移期内旧数据（测试环境，破坏性迁移直接丢弃）

## Decisions

### D1: 节点镜像存在性通过 PVE 存储内容 API 扫描

新增 `pve.Client` 方法 `ListStorageContent`（`GET /nodes/{node}/storage/local/content?content=import`），返回文件名与 volid（如 `local:import/debian-12-genericcloud-amd64.qcow2`）。不使用 SSH/文件系统直读，复用 API 令牌体系，且自定义 dir 存储路径时依然正确。

### D2: "已下载"判定 = 文件名匹配 download_url 尾部

对每个节点扫描得到文件名集合，与 `download_url` URL 路径尾段（`path.Base` 语义）比较；相等即"已下载"。手工 scp 到节点 import 目录的同名文件自动被识别，无需登记。

### D3: 创建 VM 改为镜像感知的节点选择

- 删除 `CreateVM` 中基于 node_images 的交集前置校验（`vm_service.go:318-326`）
- `selectPoolAndNode(ctx, zoneID)` 增加镜像参数：对每个池的候选节点，先按扫描结果过滤"该节点存在该镜像"，剩余节点再走可达性探测；候选为空则跳过该池
- 失败区分：区域内无任何启用节点存在该镜像 → `image_not_available`；镜像存在但全部候选节点不可达（或扫描失败无法确认镜像存在）→ `node_unavailable`（与实现一致，spec 支持）
- 顺序上先做镜像过滤再做可达性探测：content API 快于 ping 探测，且失败模式清晰

### D4: import-from 来源切换为 volid

`provisionVM` 不再从 `image.NodeImages` 取路径，改为接收创建时扫描得到的镜像 volid（D3 过滤阶段顺带获得），`DiskImportString(storageType.PVEStorage, volid)`。PVE 的 `import-from` 接受 volid 形式，与现有路径形式等价。

### D5: 下载任务落库 image_operations（沿用 vm_operations 模式）

```
CREATE TABLE image_operations (
    id            BIGSERIAL PRIMARY KEY,
    image_id      BIGINT NOT NULL REFERENCES images(id),
    node_id       BIGINT NOT NULL REFERENCES pve_nodes(id),
    action        TEXT NOT NULL,                 -- 'download'
    result        TEXT NOT NULL,                 -- 'running'/'success'/'failed'
    error_message TEXT NOT NULL DEFAULT '',
    user_id       BIGINT,                          -- 预留列：用户体系启用前恒 NULL
    upid          TEXT,                            -- PVE 任务 ID
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

每节点一条记录。`action` 常量预留扩展（未来可能支持 delete 等），`result` 沿用 vm_operations 的取值风格。与 `vm_operations` 的差异：无 pve_vmid（镜像不绑定 VM），增加 upid 便于排查。

### D6: 下载编排

`ImageService` 注入节点仓库与 PVE 客户端工厂（复用 `VMService.SetClientFactory` 模式，便于 e2e 注入 fake）。`POST /images/{id}/download` 请求体为 `{node_ids: [...]}` 或 `{zone_id: ...}` 二选一：

1. 校验镜像存在；解析目标节点集合（zone 模式取区域启用节点）
2. 为每个节点开启独立 goroutine：插入 `result='running'` 记录 → `client.DownloadURL(node, "local", "import", filename, download_url)` 得 UPID → `WaitTask` 轮询 → 更新 `result='success'/'failed'` + `error_message`
3. 使用 `context.Background()`（不借用请求 ctx）+ 超时，与 provisionVM 同模式；goroutine 内 panic recover
4. 响应 202，返回本次创建的 operation 记录列表（前端可轮询 `GET /images/{id}/operations`）

### D7: API 形态（REST）

- `POST /images` 请求体变为 `{name, default_user, download_url}`（node_images 移除，**BREAKING**）
- `GET /images?zone_id=X`：可用性语义从交集放宽为存在性（D3 配套），响应中每个镜像携带 `nodes: [{node_id, node_name, pve_name, downloaded, volid}]`（该区域启用节点的状态数组，服务层扫描聚合）
- `GET /images/{id}/nodes-status?zone_id=`：单个镜像在各启用节点的存在状态（列表页逐镜像调用）
- `POST /images/{id}/download`：202 + operations 列表（D6）
- `GET /images/{id}/operations`：下载历史（分页，复用现有分页模式）
- `ListImagesByZone` 返回类型改为携带状态数组的包装结构（handler 层序列化）

### D8: e2e fake PVE 补齐端点

fake 服务器新增 `GET /nodes/{n}/storage/local/content`（按预置文件清单返回）与 `POST /nodes/{n}/storage/local/download-url`（异步任务 + 更新文件清单），保证 e2e 与既有测试模式一致。

## Risks / Trade-offs

- [节点状态是实时扫描：节点不可达时列表页该节点状态缺失/超时] → 每节点独立超时（复用 client 默认 30s 超时），单节点失败不拖垮整个列表；状态数组允许单节点 error 标记
- [扫描并发放大：区域 N 节点 = N 次 content API] → 节点扫描并行化（goroutine + WaitGroup），区域节点通常个位数，量级可接受
- [创建时扫描到镜像、provision 时文件被删的竞争窗口] → 与现有"存储上文件被移走"同一类失败，provision 失败路径已存在（failProvision + provision_error 记录），不新增处理
- [download-url 同步返回 UPID 但任务可能立即失败] → WaitTask 轮询已覆盖终态读取；任务失败以 error_message 落库
- [download_url 尾部文件名与节点文件不同名（如 URL 带查询串）] → D2 使用 path.Base 且要求管理员填完整固定 URL；URL 解析失败视为参数错误

## Migration Plan

破坏性迁移（测试环境，无兼容期）：

1. `database/migration/0009_*.sql`：`ALTER TABLE images DROP COLUMN node_images`、`ADD COLUMN download_url TEXT NOT NULL`（迁移时旧行需填值——测试环境直接以空串占位后手工重建，或迁移脚本先用 '{}' 相关占位；具体以"镜像目录清零重建"为准），新建 `image_operations` 表
2. 全量跑通单测、e2e、`docs/openapi.yaml` lint（双副本字节一致）
3. 回滚：删除迁移即可（测试环境无数据价值）
