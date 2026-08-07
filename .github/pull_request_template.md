## 变更说明

<!-- 本次变更做了什么，为什么 -->

## 变更类型

- [ ] 新功能
- [ ] Bug 修复
- [ ] 重构
- [ ] 文档 / 配置
- [ ] 其他

## 接口契约红线（涉及 API 变更时必须勾选）

- [ ] 已同步更新 `docs/openapi.yaml`（权威源）
- [ ] 已同步复制 `api/swagger/openapi.yaml`（Embed 挂载副本，与权威源字节一致）
- [ ] 新增/修改的 operationId 完整，错误码与 `docs/api-errors.md` 一致（新增错误码属破坏性变更）
- [ ] 已通过契约校验：`npx --yes @redocly/cli lint docs/openapi.yaml`
- [ ] 已通过前端契约一致性校验（如涉及）：`web` 下 `npm run api:check`

## 测试

- [ ] 单测：`go test -count=1 ./...`
- [ ] PG 集成测试（如涉及）：`SPARK_TEST_DSN='postgres://spark:spark@127.0.0.1:5432/spark_test' go test -tags=pg ./repository/ -count=1 -run TestPG -v`（DSN 已内联，可按需覆盖，见 README「测试」）
- [ ] 端到端测试（如涉及 PVE 客户端）：`SPARK_E2E_DSN='postgres://spark:spark@127.0.0.1:5432/spark_test' go test -tags=e2e ./e2e/ -count=1 -v`（DSN 已内联，可按需覆盖，见 README「测试」）

## 安全检查

- [ ] 未引入明文敏感信息落库 / 进日志（密码、token 等）
- [ ] 错误消息对外脱敏
- [ ] 敏感字段加密符合 ADR 0004 约定

## 关联项

- openspec 变更：<!-- 若走规格驱动流程，填写变更 ID -->
- 相关 Issue：#<!-- 如有 -->

## 备注
