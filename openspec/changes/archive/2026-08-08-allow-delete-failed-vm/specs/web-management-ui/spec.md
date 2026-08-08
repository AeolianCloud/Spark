## MODIFIED Requirements

### Requirement: VM 生命周期操作

系统 SHALL 对已创建 VM 提供启动、停止、重启、调整大小与销毁操作；销毁 MUST 要求二次确认；操作失败时展示后端返回的错误信息。供给失败（status=failed）的 VM SHALL 仅可执行销毁（供给中 creating 状态与供给失败状态下均不可执行启动、停止、重启与调整大小），销毁入口对 failed 状态 MUST 可用且同样要求二次确认。

#### Scenario: 销毁前确认

- **WHEN** 运维人员点击销毁按钮
- **THEN** 界面弹出确认对话框，确认后才发出销毁请求

#### Scenario: 销毁供给失败的 VM

- **WHEN** 运维人员对供给失败（status=failed）的 VM 点击销毁按钮并二次确认
- **THEN** 界面发出销毁请求并展示结果；销毁按钮对 failed 状态可用，对供给中（creating）状态仍禁用

#### Scenario: 操作失败

- **WHEN** 后端拒绝操作（如对运行中 VM 执行冲突操作）
- **THEN** 界面展示后端返回的错误信息且 VM 状态不变
