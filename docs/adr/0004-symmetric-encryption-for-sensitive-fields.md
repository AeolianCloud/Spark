# 敏感字段应用层加密（VM 密码已实现，节点令牌待补齐）

VM 的初始密码已在入库前用 AES-256-GCM 加密（crypto 包，vm_service 层调用），错误消息对外脱敏；**节点 API 令牌目前仍明文落库**（zone_service 创建/更新节点直接写库，repository 未加密），待实现。本 ADR 的完整目标是所有敏感字段（VM 密码、节点 API 令牌）加密落库。

密钥由 `config/config.yaml` 的 `crypto.encryption_key`（base64 编码的 32 字节 AES 密钥）或 `SPARK_CRYPTO_ENCRYPTION_KEY` 环境变量提供；示例密钥仅用于开发，生产必须覆盖（配置校验对示例值告警）。
