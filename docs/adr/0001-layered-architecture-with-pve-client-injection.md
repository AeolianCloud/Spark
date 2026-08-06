# 五层架构与 PVE 客户端注入

API(gin) → service → repository → database 的严格分层，domain 模型集中在 model 包；handler 不做业务逻辑，service 层通过函数注入（如 `api.WithVMClientFactory`）接收 PVE 客户端工厂，使 e2e 测试能以内存 fake PVE 服务器（真实 host:port + `pve.WithPort`）完整走通 HTTP 链路，避免对真实 PVE 的依赖。

## Considered Options

- 仓库层返回内存 fake 而非接口注入：接口注入保留真实网络路径，e2e 更能验证协议行为
- 直接依赖真实 PVE 集群测试：环境不可控，无法进 CI
