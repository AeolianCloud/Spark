## 1. 注释中文化（纯注释改动，零行为变更）

- [x] 1.1 pve 包全部注释翻译为中文（含 pve 测试文件），git diff 校验仅注释变化
- [x] 1.2 repository 包全部注释翻译为中文（含 pg/pgxmock 测试文件），git diff 校验仅注释变化
- [x] 1.3 service 包全部注释翻译为中文（含测试文件），git diff 校验仅注释变化
- [x] 1.4 api 包（handlers/middleware/router）全部注释翻译为中文（含测试文件），git diff 校验仅注释变化
- [x] 1.5 model/database/config/crypto/cmd 包全部注释翻译为中文（含测试文件），git diff 校验仅注释变化
- [x] 1.6 e2e 包全部注释翻译为中文，git diff 校验仅注释变化
- [x] 1.7 全量回归验证：go build/go vet/全量单测/-tags=pg/-tags=e2e 全部通过，git diff 断言无代码行为变化
- [x] 1.8 仓库 AGENTS.md 增加"代码注释一律使用中文"规则条目

## 2. OpenAPI 契约校验与修复

- [x] 2.1 docs/openapi.yaml 补全 `/healthz` 的 operationId（healthz），并核对其余 24 个 operationId 语义正确
- [x] 2.2 运行 `npx @redocly/cli lint docs/openapi.yaml`，修复全部 error 级问题（warning 级记录处理说明），达到零错误
- [x] 2.3 修复后复核契约与代码一致性（端点/响应/错误码枚举与 api/router.go、docs/api-errors.md 一致）

## 3. OpenAPI 工具链验证

- [x] 3.1 引入 swaggest/swgui 依赖，注册 `GET /docs` 路由（embed openapi.yaml 渲染 Swagger UI），补路由测试
- [x] 3.2 运行 `npx openapi-typescript docs/openapi.yaml` 生成 TypeScript 类型到 /tmp，断言生成成功且输出非空（生成物不落仓库）
- [x] 3.3 README 增加 /docs 在线浏览入口与契约校验（redocly lint）、生成（openapi-typescript）命令说明

## 4. 集成回归

- [x] 4.1 全量验证：go build/go vet/全量单测/-tags=pg/-tags=e2e 全部通过
- [x] 4.2 路由一致性复核：docs/openapi.yaml 与 gin 路由表逐项核对无偏差（含 /docs 不进入契约自指）
- [x] 4.3 文档与代码契约最终核对（docs/api-errors.md、README、openapi.yaml 一致）
