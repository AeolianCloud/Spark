package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"strings"

	"spark/config"
	"spark/database"
	"spark/repository"
	"spark/service"
)

// maxAdminPasswordBytes 是管理员密码的最大字节数：与 bcrypt 的 72 字节
// 输入上限对齐（service 层对用户密码使用同一上限，见 service/user_service.go
// 的 maxPasswordBytes）。按字节而非字符计：bcrypt 处理的是输入字节序列。
const maxAdminPasswordBytes = 72

// adminCreateUsage 是 admin create 子命令的用法说明（flag 出错与 -h 时
// 输出；密码绝不作为默认值/示例出现在其中）。
const adminCreateUsage = "用法: spark admin create --username <用户名> --password <密码>"

// runAdminCommand 分发 admin 子命令（设计 D7）：当前仅支持 create，其余
// 子命令返回带用法的错误。
func runAdminCommand(args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("unknown admin subcommand; " + adminCreateUsage)
	}
	return runAdminCreate(args[1:])
}

// runAdminCreate 创建种子管理员（设计 D7）。流程与服务启动一致：加载
// 配置（auth.jwt_secret 等必填校验）→ 连接数据库 → 执行迁移（保证
// admins 表存在）→ 校验参数 → bcrypt 哈希（复用 service.HashPassword）
// → 插入 admins 表。密码绝不打印、绝不记录；重复创建同名管理员返回
// 友好冲突提示，命令整体以非零码退出。
func runAdminCreate(args []string) error {
	fs := flag.NewFlagSet("admin create", flag.ContinueOnError)
	username := fs.String("username", "", "管理员用户名（必填）")
	password := fs.String("password", "", "管理员密码（必填，绝不打印）")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), adminCreateUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		// flag 已输出错误与用法；-h/--help 视为正常退出（退出码 0）。
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q; %s", fs.Arg(0), adminCreateUsage)
	}

	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	setupLogger(cfg.Log.Level)

	ctx := context.Background()
	pool, err := database.New(ctx, cfg.Database.DSN)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := database.Migrate(ctx, pool, database.MigrationFS); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	// 参数校验风格与 service.CreateUser 一致（user_service.go）：
	// username 裁剪空白后必填、password 必填且不超过 bcrypt 72 字节上限。
	u := strings.TrimSpace(*username)
	if err := validateAdminCreate(u, *password); err != nil {
		return err
	}

	hash, err := service.HashPassword(*password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	admin, err := repository.NewAdminRepository(pool).CreateAdmin(ctx, u, hash)
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return fmt.Errorf("admin %q already exists", u)
		}
		return fmt.Errorf("create admin: %w", err)
	}
	slog.Info("admin created", "username", admin.Username)
	return nil
}

// validateAdminCreate 校验管理员创建参数（与 service/user_service.go 的
// CreateUser 校验风格一致）：username 必填（调用方已裁剪空白）、password
// 必填且不超过 bcrypt 72 字节上限。错误消息不携带任何密码内容。
func validateAdminCreate(username, password string) error {
	if username == "" {
		return errors.New("username is required")
	}
	if password == "" {
		return errors.New("password is required")
	}
	if len([]byte(password)) > maxAdminPasswordBytes {
		return fmt.Errorf("password must not exceed %d bytes", maxAdminPasswordBytes)
	}
	return nil
}
