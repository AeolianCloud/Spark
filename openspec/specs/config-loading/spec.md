## Purpose

定义配置加载行为：默认值、config.yaml、本地环境文件（.env.local）与进程环境变量的合并优先级，保证本地启动行为一致。

## ADDED Requirements

### Requirement: 配置加载优先级

系统 SHALL 按「内置默认值 → config.yaml → .env.local 本地环境文件 → 进程环境变量」的优先级合并配置，后者的值覆盖前者。系统 SHALL 在启动时自动加载仓库根目录的 `.env.local`（若存在），无需手动 source。显式设置的进程环境变量 SHALL 优先于 `.env.local` 中的同名变量。`.env.local` 文件缺失时 SHALL 静默跳过，不视为错误。

#### Scenario: 自动加载 .env.local

- **WHEN** 启动时仓库根目录存在 `.env.local` 且进程环境未设置其中的变量
- **THEN** 系统按 `.env.local` 中的键值加载配置，行为与手动 source 一致

#### Scenario: 进程环境变量优先

- **WHEN** 同一变量在进程环境与 `.env.local` 中都存在
- **THEN** 进程环境变量的值生效，`.env.local` 不覆盖它

#### Scenario: .env.local 缺失

- **WHEN** 仓库根目录不存在 `.env.local`
- **THEN** 系统正常加载其余配置源，不报错

#### Scenario: 解析规则

- **WHEN** `.env.local` 包含空行、`#` 注释行或带引号的键值
- **THEN** 空行与注释被忽略，引号（单/双）被剥离后作为值使用
