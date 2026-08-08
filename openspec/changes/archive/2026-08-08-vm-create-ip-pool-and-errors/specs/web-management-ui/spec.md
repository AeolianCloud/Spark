## MODIFIED Requirements

### Requirement: VM 创建

系统 SHALL 提供 VM 创建表单，包含名称、vCPU、内存、磁盘、镜像、存储类型、可用区、注入密码与可选 IP 池字段；密码字段为必填且仅提交一次，界面 MUST NOT 存储或回显已提交的密码。IP 池字段 SHALL 为可选下拉：选择可用区后联动加载该可用区的 IP 池列表，未选择时提交不带 pool_id（由后端自动选池）。所选可用区没有任何 IP 池时，界面 SHALL 展示"该区域未配置 IP 池"的提示并引导运维到 IP 池页面配置，仍允许提交以获取后端精确错误。创建失败时界面 SHALL 展示后端返回的错误信息。

#### Scenario: 创建 VM

- **WHEN** 运维人员填写完整表单（IP 池可选）并提交
- **THEN** 系统调用创建 API，成功后跳转至 VM 列表并可见新 VM（provision 中状态）

#### Scenario: 可选指定 IP 池

- **WHEN** 运维人员在表单中选择可用区，联动加载出的 IP 池下拉中任选一个
- **THEN** 提交时请求携带所选 pool_id

#### Scenario: 不选 IP 池提交

- **WHEN** 运维人员不选择 IP 池直接提交
- **THEN** 提交时请求不携带 pool_id，由后端自动选择 IP 池

#### Scenario: 区域无 IP 池提示

- **WHEN** 所选可用区没有任何 IP 池
- **THEN** 界面在 IP 池下拉处展示"该区域未配置 IP 池"提示并给出配置入口引导
