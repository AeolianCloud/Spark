package repository

import (
	"context"

	"spark/model"
)

// AuthRepository 提供双身份登录与鉴权中间件所需的账号查询（设计 D3/D4）。
// 它只覆盖登录/鉴权的最小查询集（按用户名/ID 取账号及密码哈希、启用状态）；
// 任务 5.x 将新增独立的 UserRepository 提供用户 CRUD（创建/列表/详情/更新/
// 删除/状态切换），本仓储保持只读查询职责不变。
type AuthRepository struct {
	pool pgxQuerier
}

// NewAuthRepository 创建由 pool 支撑的 AuthRepository。
func NewAuthRepository(pool pgxQuerier) *AuthRepository {
	return &AuthRepository{pool: pool}
}

// adminCols 是 admins 表查询的列集合。PasswordHash 仅用于登录校验，
// 序列化由 model.Admin 的 json:"-" 兜底，查询本身必须取回该列。
const adminCols = "id, username, password_hash, created_at, updated_at"

// userCols 是 users 表查询的列集合。PasswordHash 仅用于登录校验；
// Status 供登录与鉴权判断启用状态（设计 D4）。
const userCols = "id, username, password_hash, name, status, created_at, updated_at"

// GetAdminByUsername 按用户名返回管理员（含 password_hash）；账号不存在时
// 返回 pgx.ErrNoRows。供管理员登录校验使用（设计 D4）。
func (r *AuthRepository) GetAdminByUsername(ctx context.Context, username string) (*model.Admin, error) {
	var a model.Admin
	err := r.pool.QueryRow(ctx, "SELECT "+adminCols+" FROM admins WHERE username=$1", username).
		Scan(&a.ID, &a.Username, &a.PasswordHash, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &a, nil
}

// GetAdminByID 按 id 返回管理员；不存在时返回 pgx.ErrNoRows。供鉴权中间件
// 校验管理员令牌对应账号仍存在（admins 表无 status 列，存在即有效，
// 设计 D4）。
func (r *AuthRepository) GetAdminByID(ctx context.Context, id int64) (*model.Admin, error) {
	var a model.Admin
	err := r.pool.QueryRow(ctx, "SELECT "+adminCols+" FROM admins WHERE id=$1", id).
		Scan(&a.ID, &a.Username, &a.PasswordHash, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &a, nil
}

// GetUserByUsername 按用户名返回用户（含 password_hash 与 status）；账号
// 不存在时返回 pgx.ErrNoRows。供用户登录校验使用（设计 D4）。
func (r *AuthRepository) GetUserByUsername(ctx context.Context, username string) (*model.User, error) {
	var u model.User
	err := r.pool.QueryRow(ctx, "SELECT "+userCols+" FROM users WHERE username=$1", username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Name, &u.Status, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &u, nil
}

// GetUserByID 按 id 返回用户；不存在时返回 pgx.ErrNoRows。供鉴权中间件
// 校验用户令牌对应账号仍存在且启用（status=enabled，设计 D4/D5）。
func (r *AuthRepository) GetUserByID(ctx context.Context, id int64) (*model.User, error) {
	var u model.User
	err := r.pool.QueryRow(ctx, "SELECT "+userCols+" FROM users WHERE id=$1", id).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Name, &u.Status, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &u, nil
}
