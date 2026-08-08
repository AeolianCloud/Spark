# web-auth Specification

## Purpose

为 Spark 管理界面提供前端登录能力：管理员账号密码登录、JWT 令牌持久化、API 请求统一携带令牌、未登录跳转与登出，使界面可在后端强制鉴权下正常使用。

## Requirements

### Requirement: 登录页

系统 SHALL 提供登录页面（`/login`），包含管理员账号与密码输入表单，提交后调用 `POST /auth/admin/login` 完成登录；登录成功 SHALL 持久化返回的 admin JWT 令牌并跳转至首页，登录失败 SHALL 展示后端返回的认证错误信息且不跳转。

#### Scenario: 登录成功

- **WHEN** 运维人员在登录页输入正确的管理员账号与密码并提交
- **THEN** 系统调用管理员登录接口，持久化令牌并跳转至管理界面首页

#### Scenario: 登录失败

- **WHEN** 运维人员提交错误的管理员账号或密码
- **THEN** 系统展示后端返回的认证失败错误信息，停留登录页且不持久化任何令牌

### Requirement: 令牌持久化与身份状态

系统 SHALL 将登录成功返回的 JWT 令牌与管理员身份信息持久化保存（localStorage）；刷新页面后 SHALL 依据持久化令牌恢复登录状态。系统 SHALL 提供登录状态查询能力供界面判断用户是否已登录。

#### Scenario: 刷新保持登录

- **WHEN** 已登录的管理员刷新浏览器页面
- **THEN** 界面依据持久化的令牌维持登录状态，无需重新登录

#### Scenario: 未登录状态

- **WHEN** 浏览器无持久化令牌
- **THEN** 界面判定为未登录并导向登录页

### Requirement: API 请求令牌注入

系统 SHALL 在调用所有业务接口时自动携带持久化的 JWT 令牌（`Authorization: Bearer <token>`）；登录接口自身与健康检查接口 MUST NOT 携带令牌。

#### Scenario: 业务请求携带令牌

- **WHEN** 已登录的运维人员触发任意业务接口请求
- **THEN** 请求自动携带 Bearer 令牌，后端正常处理

#### Scenario: 登录请求不携带令牌

- **WHEN** 运维人员提交登录表单
- **THEN** 登录请求不携带任何已存令牌

### Requirement: 401 统一处理与登出

系统 SHALL 在任意业务接口返回 401 unauthorized 时清除本地令牌并跳转登录页。系统 SHALL 提供登出入口，退出时清除本地令牌并跳转登录页；登出后访问任意业务页面 SHALL 被导向登录页。

#### Scenario: 令牌失效跳转登录

- **WHEN** 已登录管理员的令牌过期或被后端拒绝（接口返回 401）
- **THEN** 系统清除本地令牌并跳转登录页

#### Scenario: 主动登出

- **WHEN** 已登录管理员点击登出入口
- **THEN** 系统清除本地令牌并跳转登录页

#### Scenario: 未登录访问业务页

- **WHEN** 未登录用户在浏览器中直接访问业务页面路由
- **THEN** 系统导向登录页

### Requirement: 登录页访问控制

已登录管理员访问登录页 SHALL 被导向首页，不得重复登录。

#### Scenario: 已登录访问登录页

- **WHEN** 已登录管理员打开登录页路由
- **THEN** 系统自动跳转至首页
