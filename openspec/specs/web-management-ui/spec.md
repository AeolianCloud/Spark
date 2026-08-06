## Purpose

为 Spark 平台运维人员提供可视化的内部管理界面，覆盖可用区、节点、IP 池、存储类型、镜像与虚拟机的完整读写管理，替代 curl 与 Swagger UI 手工操作。

## Requirements

### Requirement: 管理界面整体形态
系统 SHALL 提供基于 Web 浏览器的内部管理界面，界面文案为中文，通过浏览器访问使用；界面读取的所有数据与执行的全部操作 MUST 经由后端 API 契约（`docs/openapi.yaml`）定义的能力完成，MUST NOT 绕过 API 直接访问数据库或 PVE。

#### Scenario: 访问管理界面
- **WHEN** 运维人员在浏览器中打开管理界面地址
- **THEN** 界面正常加载并展示 Dashboard 概览

#### Scenario: 界面数据源
- **WHEN** 界面展示任何资源数据或执行任何资源操作
- **THEN** 其请求均调用后端 API 端点且请求/响应结构与 `docs/openapi.yaml` 契约一致

### Requirement: 无鉴权访问
第一批变更中，管理界面及后端 API SHALL 不要求登录即可访问与操作；该行为为已知权衡，部署方 MUST 通过网络层限制访问范围。

#### Scenario: 直接访问
- **WHEN** 任意用户打开管理界面并执行任意资源操作
- **THEN** 操作直接生效，无需任何登录或凭证输入

### Requirement: Dashboard 资源概览
系统 SHALL 在首页展示平台资源概览：Zone 总数、Node 总数、VM 总数（按运行状态区分）、IP 池地址占用情况。

#### Scenario: 展示概览
- **WHEN** 运维人员打开 Dashboard 页面
- **THEN** 页面展示上述资源统计，且数据来自各资源的列表/详情 API

### Requirement: Zone 管理
系统 SHALL 支持 Zone 的列表、创建与删除；删除仅在后端允许时成功，失败时展示后端返回的错误信息。

#### Scenario: 创建 Zone
- **WHEN** 运维人员填写 Zone 名称并提交创建
- **THEN** 系统调用创建 API，成功后新 Zone 出现在列表中

#### Scenario: 删除 Zone 失败
- **WHEN** Zone 下仍存在节点且运维人员尝试删除
- **THEN** 系统展示后端返回的冲突错误信息且 Zone 保持不变

### Requirement: Node 管理
系统 SHALL 支持 Node 的列表与创建编辑；节点 API 令牌为只写字段：表单可输入新令牌，但列表与详情 MUST NOT 回显令牌本身，仅展示是否已配置（api_token_set）。

#### Scenario: 创建节点
- **WHEN** 运维人员填写节点名称、主机地址、API 用户与令牌并提交
- **THEN** 系统调用创建 API，成功后新节点出现在所属 Zone 的节点列表中

#### Scenario: 令牌只写
- **WHEN** 运维人员查看节点详情或列表
- **THEN** 界面仅展示"已配置令牌"状态，不展示令牌内容

### Requirement: IP 池管理
系统 SHALL 支持 IP 池的列表、创建、更新与删除，并支持配置池内允许分配 IP 的节点白名单（setPoolNodes）。

#### Scenario: 配置池节点白名单
- **WHEN** 运维人员在 IP 池详情页勾选允许分配的节点并保存
- **THEN** 系统调用节点绑定 API，成功后白名单状态与后端一致

### Requirement: 存储类型管理
系统 SHALL 支持存储类型的列表、创建、编辑与删除；删除失败（如被引用）时展示后端错误。

#### Scenario: 删除被引用的存储类型
- **WHEN** 存在使用该存储类型的 VM 且运维人员尝试删除
- **THEN** 系统展示后端返回的冲突错误信息且存储类型保持不变

### Requirement: 镜像管理
系统 SHALL 支持镜像的列表与创建；创建表单包含镜像名、默认登录用户及各节点上的镜像路径。

#### Scenario: 创建镜像
- **WHEN** 运维人员填写镜像名、默认用户并指定至少一个节点上的路径后提交
- **THEN** 系统调用创建 API，成功后新镜像出现在镜像列表中

### Requirement: VM 列表与详情
系统 SHALL 支持 VM 的分页列表与详情查看；列表按后端分页契约展示（limit/offset 与 X-Total-Count 总数），每行展示 VM 名称、规格、状态与所属 Zone/节点；状态为 PVE 实时透传状态，列表页 MUST 提供手动刷新能力，MUST NOT 以高于 10 秒的频率自动轮询；PVE 不可达导致状态降级时界面 MUST 展示降级提示而非伪造状态。

#### Scenario: 分页浏览
- **WHEN** VM 总数超过单页容量
- **THEN** 界面展示分页控件且总页数依据后端 X-Total-Count 计算

#### Scenario: 手动刷新状态
- **WHEN** 运维人员点击刷新按钮
- **THEN** 界面重新请求列表并展示最新实时状态

#### Scenario: 状态降级
- **WHEN** 某 VM 所在 PVE 节点不可达
- **THEN** 该 VM 状态展示为降级后的静态字段值并附降级提示

### Requirement: VM 创建
系统 SHALL 提供 VM 创建表单，包含名称、vCPU、内存、磁盘、镜像、存储类型、可用区与注入密码字段；密码字段为必填且仅提交一次，界面 MUST NOT 存储或回显已提交的密码。

#### Scenario: 创建 VM
- **WHEN** 运维人员填写完整表单并提交
- **THEN** 系统调用创建 API，成功后跳转至 VM 列表并可见新 VM（provision 中状态）

### Requirement: VM 生命周期操作
系统 SHALL 对已创建 VM 提供启动、停止、重启、调整大小与销毁操作；销毁 MUST 要求二次确认；操作失败时展示后端返回的错误信息。

#### Scenario: 销毁前确认
- **WHEN** 运维人员点击销毁按钮
- **THEN** 界面弹出确认对话框，确认后才发出销毁请求

#### Scenario: 操作失败
- **WHEN** 后端拒绝操作（如对运行中 VM 执行冲突操作）
- **THEN** 界面展示后端返回的错误信息且 VM 状态不变

### Requirement: 契约一致性保障
前端 API client 代码 MUST 由 `docs/openapi.yaml` 生成（生成物入库或构建期生成），任何接口的增、删、改（尤其写操作）后 MUST 同步契约文件与生成 client，未同步契约的接口变更不允许合并。

#### Scenario: 契约变更后 client 更新
- **WHEN** 后端接口发生增、删、改且契约同步更新
- **THEN** 前端重新生成 client 后请求/响应结构与新契约一致

#### Scenario: 未同步契约的变更被拒绝
- **WHEN** 后端改动接口但未同步 `docs/openapi.yaml`
- **THEN** 该变更不允许合并（评审或 CI 拒绝）
