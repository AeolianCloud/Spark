# OpenAPI 契约与工具链（redocly 校验 + swaggest 离线 UI）

API 契约以 `docs/openapi.yaml` 为准，变更同步 `api/swagger/openapi.yaml` 挂载副本并保证 operationId 完整。契约校验由 redocly lint 完成（零错误目标）；swaggest/swgui 仅提供纯 Go embed 的离线 Swagger UI（挂载 /docs，内网可用），不引入 swag 注释约定，也不生成前端客户端代码入库（生成仅用于验证）。
