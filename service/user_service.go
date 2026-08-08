package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"spark/model"
	"spark/repository"
)

// maxPasswordBytes 是密码的最大字节数：与 bcrypt 的 72 字节输入上限对齐
// （bcrypt.GenerateFromPassword 对超长输入返回错误），超长密码在 service
// 层提前以 400 拒绝，避免落为内部错误。按字节而非字符计：bcrypt 处理
// 的是输入字节序列。
const maxPasswordBytes = 72

// KindUserHasResources 表示删除用户时其名下仍关联虚拟机等资源（设计 D6
// 的"有资源禁删"）。该值位于 errors.go 中共享 kind 的 iota 范围之外
// （该范围归其他批次所有），取值风格与 KindNodeUnavailable（100）、
// KindIPExhausted（101）一致。
const KindUserHasResources ErrorKind = 102

// userHasResourcesf 构造一个 KindUserHasResources 服务错误（映射为 409）。
func userHasResourcesf(format string, args ...any) *Error {
	return &Error{Kind: KindUserHasResources, Message: fmt.Sprintf(format, args...)}
}

// UserRepository 是 UserService 依赖的用户数据访问接口。
// repository.UserRepository 满足该接口。
type UserRepository interface {
	Create(ctx context.Context, username, passwordHash, name string) (*model.User, error)
	List(ctx context.Context, limit, offset int) ([]model.User, error)
	Count(ctx context.Context) (int, error)
	GetByID(ctx context.Context, id int64) (*model.User, error)
	Update(ctx context.Context, id int64, name *string, passwordHash *string) (*model.User, error)
	Delete(ctx context.Context, id int64) error
	SetStatus(ctx context.Context, id int64, status string) (*model.User, error)
}

// UserService 实现管理员侧用户管理的业务规则（设计 D6）：创建（username
// 全局唯一 + 初始密码）、分页列表与详情、修改（name/重置密码）、删除
// （名下有关联资源时拒绝）与启用/禁用状态切换。密码哈希复用同包的
// HashPassword（bcrypt，设计 D1）——明文密码绝不落库、绝不返回。
type UserService struct {
	userRepo UserRepository
}

// NewUserService 创建由 userRepo 支撑的 UserService。
func NewUserService(userRepo UserRepository) *UserService {
	return &UserService{userRepo: userRepo}
}

// CreateUser 创建用户：username 必填且全局唯一（重复经唯一约束映射为
// 409 冲突），password 必填且不超过 maxPasswordBytes，name 可选
// （缺省为空字符串）。返回创建的用户；密码哈希由 model.User 的
// json:"-" 兜底不外泄（handler 层另有独立响应结构双保险）。
func (s *UserService) CreateUser(ctx context.Context, username, password, name string) (*model.User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, badRequestf("username is required")
	}
	if password == "" {
		return nil, badRequestf("password is required")
	}
	if len([]byte(password)) > maxPasswordBytes {
		return nil, badRequestf("password must not exceed %d bytes", maxPasswordBytes)
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("create user: hash password: %w", err)
	}
	user, err := s.userRepo.Create(ctx, username, hash, strings.TrimSpace(name))
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, conflictf("user %q already exists", username)
		}
		return nil, fmt.Errorf("create user: %w", err)
	}
	return user, nil
}

// ListUsers 返回按 id 排序的一页用户与用户总数（total 支撑
// X-Total-Count 响应头）。
func (s *UserService) ListUsers(ctx context.Context, limit, offset int) ([]model.User, int, error) {
	users, err := s.userRepo.List(ctx, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list users: %w", err)
	}
	total, err := s.userRepo.Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list users: count: %w", err)
	}
	return users, total, nil
}

// GetUser 返回指定 id 的用户；用户不存在时返回 404 not_found。
func (s *UserService) GetUser(ctx context.Context, id int64) (*model.User, error) {
	user, err := s.userRepo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("user %d not found", id)
		}
		return nil, fmt.Errorf("get user %d: %w", id, err)
	}
	return user, nil
}

// UpdateUser 应用用户的局部更新：name 与 password 至少提供一个。
// password 用于重置密码，校验规则与 CreateUser 一致（必填非空、不
// 超过 maxPasswordBytes），bcrypt 哈希后落库——空串会生成合法 bcrypt
// 哈希（等价"无密码登录"后门，S3），必须在 service 层拒绝。返回更新后的
// 用户；用户不存在时返回 404 not_found。
func (s *UserService) UpdateUser(ctx context.Context, id int64, name, password *string) (*model.User, error) {
	if name == nil && password == nil {
		return nil, badRequestf("at least one of name or password is required")
	}
	var nameVal *string
	if name != nil {
		trimmed := strings.TrimSpace(*name)
		nameVal = &trimmed
	}
	var hashVal *string
	if password != nil {
		if *password == "" {
			return nil, badRequestf("password is required")
		}
		if len([]byte(*password)) > maxPasswordBytes {
			return nil, badRequestf("password must not exceed %d bytes", maxPasswordBytes)
		}
		hash, err := HashPassword(*password)
		if err != nil {
			return nil, fmt.Errorf("update user %d: hash password: %w", id, err)
		}
		hashVal = &hash
	}
	user, err := s.userRepo.Update(ctx, id, nameVal, hashVal)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("user %d not found", id)
		}
		return nil, fmt.Errorf("update user %d: %w", id, err)
	}
	return user, nil
}

// DeleteUser 删除指定 id 的用户。名下仍关联虚拟机等资源（vms.user_id
// 外键引用）时返回 KindUserHasResources（设计 D6 的"有资源禁删"）；
// 用户不存在时返回 404 not_found。删除不影响其名下已存的操作记录
// （vm_operations 仅存 ID 引用，无级联）。
func (s *UserService) DeleteUser(ctx context.Context, id int64) error {
	err := s.userRepo.Delete(ctx, id)
	switch {
	case errors.Is(err, repository.ErrInUse):
		return userHasResourcesf("user %d has resources", id)
	case errors.Is(err, pgx.ErrNoRows):
		return notFoundf("user %d not found", id)
	case err != nil:
		return fmt.Errorf("delete user %d: %w", id, err)
	}
	return nil
}

// SetUserStatus 切换用户的启用状态（enabled/disabled）：非法取值返回
// 400 bad_request，用户不存在返回 404 not_found。禁用后该用户的登录与
// 已签发令牌的操作由登录服务与鉴权中间件按 status 即时拒绝（任务 3/4
// 已实现），本方法只做状态切换本身。
func (s *UserService) SetUserStatus(ctx context.Context, id int64, status string) (*model.User, error) {
	if status != model.UserStatusEnabled && status != model.UserStatusDisabled {
		return nil, badRequestf("invalid status %q: must be enabled or disabled", status)
	}
	user, err := s.userRepo.SetStatus(ctx, id, status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("user %d not found", id)
		}
		return nil, fmt.Errorf("set user %d status: %w", id, err)
	}
	return user, nil
}
