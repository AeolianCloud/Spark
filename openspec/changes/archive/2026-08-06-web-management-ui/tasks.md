## 1. 脚手架与基础配置

- [x] 1.1 初始化 Nuxt 4 + Nuxt UI v4 于 `web/` 目录，锁定 Node 版本与包管理器（design D2、Open Questions），产出 package.json/lockfile
- [x] 1.2 配置 Nuxt dev proxy：`/api` 代理到本地后端（:8080），并验证代理可用（design D8）
- [x] 1.3 建立基础布局（侧边导航 + 内容区），列出全部管理模块入口：Dashboard、Zones、Nodes、IP Pools、Storage Types、Images、VMs
- [x] 1.4 约定统一错误展示组件（后端错误码/消息脱敏展示）与加载/空态样式

## 2. API Client 契约生成

- [x] 2.1 在 orval 与 openapi-typescript 中选型，配置从 `docs/openapi.yaml` 生成 TS client（design D3）
- [x] 2.2 首次生成 client 并入库，校验生成结果与契约一致（25 个端点全覆盖、类型完整）
- [x] 2.3 添加 CI 步骤：契约变更后重新生成 client 并 diff 校验，未同步契约的变更被拒绝（specs - 契约一致性保障）
- [x] 2.4 验证写操作 client 封装：createVM/resizeVM/destroyVM 等请求体与响应体类型与契约一致

## 3. 基础设施管理页面

- [x] 3.1 Dashboard：Zone/Node/VM 统计与 IP 池占用概览，数据全部来自列表/详情 API（specs - Dashboard）
- [x] 3.2 Zones 列表与创建（删除因契约无 DELETE /zones 端点未实现，页面已标注缺口）（specs - Zone 管理）
- [x] 3.3 Nodes 列表与创建/编辑：令牌只写不读、api_token_set 状态回显（specs - Node 管理、design D7）
- [x] 3.4 IP Pools 列表/创建 + 池内节点白名单配置（更新/删除因契约无 PUT/DELETE /ip-pools/{id} 端点未实现，页面已标注缺口）（specs - IP 池管理）
- [x] 3.5 Storage Types 列表/创建/编辑/删除（含被引用冲突错误展示）（specs - 存储类型管理）
- [x] 3.6 Images 列表与创建（镜像名/默认用户/节点路径）（specs - 镜像管理）

## 4. VM 管理页面

- [x] 4.1 VM 列表：limit/offset 分页 + X-Total-Count 总数、状态徽章、URL query 驱动页码（specs - VM 列表与详情、design D6）
- [x] 4.2 VM 列表刷新策略：手动刷新按钮 + 可选 ≥10s 低频轮询；PVE 降级时展示降级提示（design D5）
- [x] 4.3 VM 详情页：规格、实时状态、所属 Zone/节点信息展示
- [x] 4.4 VM 创建表单：名称/vCPU/内存/磁盘/镜像/存储类型/可用区/密码，密码一次性提交不存储不回显（specs - VM 创建、design D7）
- [x] 4.5 VM 生命周期操作：start/stop/restart/resize/destroy，销毁二次确认，失败展示后端错误（specs - VM 生命周期操作）

## 5. 联调与验收

- [x] 5.1 本地全链路联调：后端 + 前端 + fake PVE/真实 PVE，覆盖每个页面读与写
- [x] 5.2 后端回归：`go test` 与 `go test -tags=e2e ./e2e/ -count=1 -v` 全部通过
- [x] 5.3 契约一致性终检：前端 client 与 `docs/openapi.yaml` 完全对齐（生成 diff 为空）
- [x] 5.4 部署文档：nginx 静态托管 `web/` 构建产物 + `/api` 反代、无鉴权下的网络隔离提示（design D8、Migration Plan）
- [x] 5.5 代码审查：reviewer 审查前端代码，涉及密码/令牌处理处由 security-reviewer 复核
