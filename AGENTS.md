# Spark 项目说明

- 这是一个 PVE 虚拟化后端项目
- 主要技术栈为 go+gin
- 主要功能包括虚拟机的全生命周期管理，对接前端实现公有云
- 严格按照 PVE 标准 API 实现（https://pve.proxmox.com/pve-docs/api-viewer/index.html）
- 代码注释一律使用中文（含测试代码），专有名词与技术术语可保留英文原文
- **PVE 版本为 9（生产环境 9.1.1）**，适配 PVE 客户端时注意以下版本差异：
  - `/nodes/{node}/status` 在 PVE 9 中无 `mem/maxmem/cpus/maxcpu/version/status` 字段，改用对象结构：`memory{total,used,free,available}`、`cpuinfo{cpus,cores,sockets,model}`、`pveversion`（替代 `version`）；`rootfs` 为对象 `{total,used,free,avail}`（PVE 8.2+ 开始），PVE 7 为裸数字，解析需双格式兼容
  - `/nodes/{node}/netstat` 在 PVE 9 中只返回 VM 网络设备计数器（`[{dev,vmid,in,out}]`），无物理网卡流量；节点级网络吞吐改用 `/nodes/{node}/rrddata?timeframe=hour` 的 `netin/netout`（bytes/s，取最后一个数据点）
  - PVE 的布尔字段（如 network 的 `active`）在 JSON 中返回数字 `1/0` 而非 `true/false`，解析需兼容两种形式

## 项目约束

- 领域术语以根目录 `CONTEXT.md` 为准，架构决策记录在 `docs/adr/`（见 ADR 0003）
- **环境变量红线**：每次新增 `SPARK_*` 环境变量（config 支持新配置项时），必须同步写入 `.env.example`（模板）与 `.env.local`（本地，gitignore 不提交），并注明生成方式与必填性；`.env.local` 缺变量会导致本地启动/测试行为与示例不一致
- **环境变量一致性核对**：新增或修改 config 配置项时，须保证三方键名一一对应——`config.yaml` 配置项 ↔ `config/config.go applyEnv` 的环境变量 ↔ `.env.example`/`.env.local` 键名；`.env` 中留空（`KEY=`）会被当作显式空值注入并覆盖 yaml 值（如白名单留空=拒绝所有下载），不能留空表示"不设置"时须删除该行或填默认值；提交前用 `grep -cE "^SPARK_" .env.example` 与 `.env.local` 核对数量一致
- 接口契约红线：任何接口的增、删、改（尤其写操作）后，必须同步更新 `docs/openapi.yaml`（权威源）与 `api/swagger/openapi.yaml`（Swagger UI 挂载副本），并保证 operationId 完整；契约是前端唯一的事实来源，未同步契约的接口变更不允许合并
- **前后端同步红线**：后端修改接口（增、删、改，含请求/响应结构、认证方式、错误码变更）时，必须同步检查前端 `web/` 并保持一致——前端 API 客户端（`web/app/api/client.ts`）、类型定义（`web/app/api/types.ts`）、页面消费逻辑须随契约同步更新；涉及前端时 PR 前须跑 `npm run api:check`（生成的 client 与契约一致、git diff 为空）。新增/变更的接口若前端暂不消费，须在变更说明中注明，不能因"前端没做"而回退后端契约
- 敏感字段加密：VM 密码已经 crypto 包（AES-256-GCM）加密后落库；节点 API 令牌加密待实现（见 ADR 0004），错误消息对外脱敏
- 端到端测试位于 `e2e/`（`go test -tags=e2e ./e2e/ -count=1 -v`），依赖 fake PVE 服务器注入，改动涉及 PVE 客户端时须保持其可用

## 开发流程

### Review 分级

按变更路径分级，核心路径从严、文档路径从宽：

| 级别 | 覆盖路径 | 审查要求 |
| --- | --- | --- |
| 核心 | `api/` `service/` `repository/` `database/` `crypto/` `pve/` `model/` `e2e/` `web/` | 必须 `reviewer` 审查；涉及敏感数据、认证鉴权、加密、输入校验时额外使用 `security-reviewer` |
| 普通 | `config/` `cmd/` `.github/` `openspec/` | `reviewer` 审查 |
| 文档 | `docs/` `*.md` `CONTEXT.md` `AGENTS.md` | `reviewer` 快速审查即可，可放宽 |

### 提交规范

- 提交消息遵循 Conventional Commits：`feat:` / `fix:` / `docs:` / `style:` / `refactor:` / `perf:` / `test:` / `chore:` / `ci:`，如 `feat: 导入 PVE 已有 VM（纳管）`
- 每次代码完成（一批并行任务结束后）必须先经 `reviewer` 审查，通过后再继续下一步
- 前端与后端子 agent 互相隔离，发现问题报告主会话统一指派

### PR 契约 checklist

提交 PR 前核对 `.github/pull_request_template.md`，必查项：

1. `docs/openapi.yaml` 与 `api/swagger/openapi.yaml` 双副本字节一致
2. `npx --yes @redocly/cli lint docs/openapi.yaml` 通过（豁免规则见 `.redocly.lint-ignore.yaml`）
3. 涉及前端时 `npm run api:check` 通过（生成的 client 与提交版本 git diff 为空）
4. 新增/修改错误码必须同步 `docs/api-errors.md`（新增错误码属破坏性变更）
