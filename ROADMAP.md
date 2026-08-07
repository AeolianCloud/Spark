# 路线图

> 本路线图基于 openspec 归档变更、ADR 决策与 README「已知限制」提炼，标注目标版本与来源，便于排期与认领。

## Phase 1 — 核心能力 ✅（已完成）

> 云主机全生命周期管理闭环 + 契约工程化。

- [x] VM 全生命周期：创建（IP 分配 → PVE 创建链）→ 详情/列表（穿透式实时状态）→ 启停重启 → 升降配 → 销毁（[2026-08-05-vm-lifecycle-v1](openspec/changes/archive/2026-08-05-vm-lifecycle-v1)）
- [x] 基础资源管理：区域（zone）、PVE 节点登记、IP 池（并发安全分配）、存储类型、镜像目录（openspec/specs 对应主规格）
- [x] PVE 节点名自动发现 + 节点自定义端口（[2026-08-06-auto-discover-pve-node-name](openspec/changes/archive/2026-08-06-auto-discover-pve-node-name)、[2026-08-06-support-node-custom-port](openspec/changes/archive/2026-08-06-support-node-custom-port)）
- [x] 导入 PVE 已有 VM（纳管，IP 优先复用静态 IP）（[2026-08-07-import-existing-vms](openspec/changes/archive/2026-08-07-import-existing-vms)）
- [x] Web 管理界面（Nuxt 4 + Nuxt UI v4，契约生成 client）（[2026-08-06-web-management-ui](openspec/changes/archive/2026-08-06-web-management-ui)）
- [x] OpenAPI 契约工具链：redocly 校验 + swagger-ui 挂载 + 前端类型生成（ADR 0003）
- [x] 全仓库中文注释（含测试与迁移）
- [x] 三层测试体系：pgxmock 单测 / `-tags=pg` PG 集成 / `-tags=e2e` fake PVE 端到端

## Phase 2 — 稳定性与安全 🚧（进行中/待排期）

> 目标：把"能用"升级为"可信"——任务可追踪、失败可恢复、权限可审计、部署可隔离。

### 任务持久化与恢复（P2 高）
- [ ] PVE 任务/供给过程落库，失败可重试、可手动重放
- [ ] 供给失败自动释放 IP 或提供回收入口（当前仅标记 `provision_error`，需运营手工处理）
- 来源：README「已知限制」
- 难度：⭐⭐⭐

### 认证鉴权（P2 高）
- [ ] API 认证方案（token / session），敏感操作权限控制
- [ ] Web 管理界面登录接入
- 来源：README「已知限制」、SECURITY.md
- 难度：⭐⭐⭐

### 节点 API 令牌加密（P2 高）
- [ ] 节点令牌经 crypto 包（AES-256-GCM）加密落库，存量明文数据迁移
- 来源：docs/adr/0004（已明确待实现）
- 难度：⭐⭐

### 操作审计日志（P2 中）
- [ ] 写操作（创建/销毁/启停/升降配）记录操作审计，可追溯
- 来源：开发流程对比复盘
- 难度：⭐⭐

### 后端 CI 门禁（P2 中）
- [ ] Go 后端 workflow：`go vet` + `go test ./...` + lint（当前仅前端有 CI）
- 来源：开发流程对比复盘
- 难度：⭐⭐

### 异步失败兜底（P2 中）
- [ ] 供给链失败后的 PVE 侧残留半成品 VM 清理机制
- 来源：README「已知限制」
- 难度：⭐⭐⭐

## Phase 3 — 能力扩展（规划中）

> 目标：从单机管理走向云化能力。

- [ ] 磁盘扩展操作：迁移、克隆、快照（当前仅支持扩容，来源：README「已知限制」）— 难度 ⭐⭐⭐
- [ ] 计费 / 配额：CPU / 内存 / 磁盘用量限制 — 难度 ⭐⭐⭐
- [ ] VM 镜像模板化（从现有 VM 生成镜像）— 难度 ⭐⭐
- [ ] 多区域容灾与迁移 — 难度 ⭐⭐⭐⭐

## 参与方式

1. 优先排期 Phase 2 高优先级项；
2. 新能力请走 openspec 流程：propose（design/specs/tasks）→ apply → archive/sync 主规格，完成后更新本路线图；
3. 接口变更必须同步 `docs/openapi.yaml` 契约（红线，见 AGENTS.md）。
