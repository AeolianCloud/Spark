## 1. PVE 客户端

- [x] 1.1 新增 `pve/status.go`：`NodeStatus`（GET /nodes/{node}/status，PVE 7/8/9 双格式：cpu/cpus/maxcpu/mem/maxmem/rootfs/maxrootfs/status/uptime/version/kversion/loadavg + PVE 9 新增 cpuinfo/memory/pveversion 对象）、`NodeNetwork`（GET /nodes/{node}/network，接口结构化列表，active 用 PveBool 兼容布尔与数字 1/0）
- [x] 1.2 新增 `pve/status_test.go`：假 HTTP 服务器覆盖三个方法成功解析、负载字段缺失容错、上游 4xx/5xx 与网络错误
- [x] 1.3 PVE 9 适配：`NodeNetStats`（netstat 文本解析）删除，替换为 `NodeNetIO`（GET /nodes/{node}/rrddata?timeframe=hour，取最后一个点 netin/netout 作为节点吞吐，空数组返回全零容错；PVE 9 实测 netstat 只返回 VM 设备计数器不可用）

## 2. Service 层

- [x] 2.1 新增 `service/node_status_service.go`：`NodeStatusService`（依赖 NodeRepository + 可注入 newClient 工厂，同 ImageService 模式），`GetStatus` 并发拉取 status/network/rrddata 并聚合；节点不存在返回 not_found 类错误，PVE 调用失败返回 `KindNodeUnavailable`
- [x] 2.2 新增 `service/node_status_service_test.go`：节点不存在、PVE 不可达降级、聚合成功、负载字段缺失容错
- [x] 2.3 PVE 9 适配：`Traffic` 字段改为 `NetIO *pve.NodeIO`；status 的 status 字段缺失（PVE 9）时补 "online"（请求成功即在线）

## 3. Handler 与路由

- [x] 3.1 新增 `api/handlers/node_status_handler.go`：`GET /nodes/:id/status`，复用 `mapServiceErrorExtended`（404/503 映射），错误消息脱敏
- [x] 3.2 在 `api/router.go` 挂载新端点（nodesGroup），handler 注释完整
- [x] 3.3 新增 `api/handlers/node_status_handler_test.go`：200 完整负载、404、503 降级
- [x] 3.4 PVE 9 适配：handler 回退链（cores：cpuinfo.cpus → cpus → maxcpu；内存：memory 对象 → maxmem/mem；版本：pveversion → version），network 删除 rx/tx 字段（active 用 PveBool 输出 boolean），新增 `net_io` 字段（nodeStatusField.NetIO）

## 4. OpenAPI 契约

- [x] 4.1 `docs/openapi.yaml` 新增 `/nodes/{id}/status` 路径与 `NodeStatusResponse`/网络接口等 schema，operationId 完整
- [x] 4.2 同步 `api/swagger/openapi.yaml` 双副本并校验字节一致
- [x] 4.3 `npx --yes @redocly/cli lint docs/openapi.yaml` 通过
- [x] 4.4 契约同步：NodeStatusField 新增 `net_io`（NodeNetIO：net_in/net_out，number，bytes/s），NodeNetStatus 删除 rx_bytes/tx_bytes（active 保持 nullable boolean），双副本字节一致
- [ ] 4.5 前端 `npm run api:check` 通过（生成的 client 已用 `npm run api:gen` 重新生成且与契约一致；api:check 的 git diff 校验需生成物提交入库后通过，待提交）

## 5. 前端

- [x] 5.1 改造 `web/app/pages/nodes.vue`：表格新增 CPU/内存列（并行请求各节点 status，降级徽标），顶部手动刷新按钮
- [x] 5.2 新增详情页 `web/app/pages/zones/[zoneId]/nodes/[nodeId].vue`：配置卡片 + 状态卡片（CPU/内存/磁盘/网络/PVE 版本与集群信息）+ 手动刷新 + 降级提示
- [x] 5.3 列表页「详情」入口链接到新详情页

## 6. 验证

- [x] 6.1 `go build ./...` 与 `go vet ./...` 通过
- [x] 6.2 后端相关单测全量通过（`go test ./pve/ ./service/ ./api/...`）
- [x] 6.3 `go test -tags=e2e ./e2e/ -count=1 -v` 通过（本机 DSN 需 `SPARK_E2E_DSN='postgres://spark:change-me@127.0.0.1:5432/spark_test'` 覆盖）
- [x] 6.4 前端 `npm run typecheck` / lint 通过
