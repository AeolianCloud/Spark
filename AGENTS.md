# Spark 项目说明

- 这是一个 PVE 虚拟化后端项目
- 主要技术栈为 go+gin
- 主要功能包括虚拟机的全生命周期管理，对接前端实现公有云
- 严格按照 PVE 标准 API 实现（https://pve.proxmox.com/pve-docs/api-viewer/index.html）
- 代码注释一律使用中文（含测试代码），专有名词与技术术语可保留英文原文

## 项目约束

- 领域术语以根目录 `CONTEXT.md` 为准，架构决策记录在 `docs/adr/`（见 ADR 0003）
- 变更必须同步两份 OpenAPI 契约：`docs/openapi.yaml`（权威源）与 `api/swagger/openapi.yaml`（Swagger UI 挂载副本），并保证 operationId 完整
- 敏感字段加密：VM 密码已经 crypto 包（AES-256-GCM）加密后落库；节点 API 令牌加密待实现（见 ADR 0004），错误消息对外脱敏
- 端到端测试位于 `e2e/`（`go test -tags=e2e ./e2e/ -count=1 -v`），依赖 fake PVE 服务器注入，改动涉及 PVE 客户端时须保持其可用
