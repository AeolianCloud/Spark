package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"spark/model"
)

// UserRepository 提供管理员侧的用户管理持久化（设计 D6）：创建/分页
// 列表/详情/更新/删除/状态切换。登录与鉴权所需的只读查询由
// AuthRepository 承担（见 auth_repo.go 的职责注释），本仓储只负责管理
// 操作，两者互不干扰。
type UserRepository struct {
	pool pgxQuerier
}

// NewUserRepository 创建由 pool 支撑的 UserRepository。
func NewUserRepository(pool pgxQuerier) *UserRepository {
	return &UserRepository{pool: pool}
}

// Create 插入一个用户并返回它（已填充 id 与时间戳）。passwordHash 必须
// 是 service 层传入的 bcrypt 哈希——明文密码绝不进入 repository 层。
// username 重复时产生 ErrConflict（SQLSTATE 23505 经 classifyDBError
// 映射）。name 为空字符串时由列默认值语义兜底（应用层显式写入空串）。
func (r *UserRepository) Create(ctx context.Context, username, passwordHash, name string) (*model.User, error) {
	var u model.User
	err := r.pool.QueryRow(ctx,
		"INSERT INTO users (username, password_hash, name) VALUES ($1, $2, $3) RETURNING "+userCols,
		username, passwordHash, name,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Name, &u.Status, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &u, nil
}

// List 返回按 id 排序的一页用户（limit/offset 分页，offset 越界时返回
// 空列表）。支撑 GET /users 分页列表端点。
func (r *UserRepository) List(ctx context.Context, limit, offset int) ([]model.User, error) {
	rows, err := r.pool.Query(ctx,
		"SELECT "+userCols+" FROM users ORDER BY id LIMIT $1 OFFSET $2", limit, offset)
	if err != nil {
		return nil, fmt.Errorf("users: list: %w", err)
	}
	defer rows.Close()

	users := make([]model.User, 0)
	for rows.Next() {
		var u model.User
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Name, &u.Status, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("users: scan: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("users: iterate: %w", err)
	}
	return users, nil
}

// Count 返回用户总数，支撑 GET /users 的 X-Total-Count 响应头。
func (r *UserRepository) Count(ctx context.Context) (int, error) {
	var n int
	if err := r.pool.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&n); err != nil {
		return 0, fmt.Errorf("users: count: %w", err)
	}
	return n, nil
}

// GetByID 返回指定 id 的用户；不存在时返回 pgx.ErrNoRows。
func (r *UserRepository) GetByID(ctx context.Context, id int64) (*model.User, error) {
	var u model.User
	err := r.pool.QueryRow(ctx, "SELECT "+userCols+" FROM users WHERE id=$1", id).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Name, &u.Status, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &u, nil
}

// Update 应用用户的局部更新：name 与 passwordHash 为 nil 的字段保持
// 原值（service 层保证至少一个非 nil）。passwordHash 必须是 bcrypt
// 哈希。返回更新后的行；id 不存在时返回 pgx.ErrNoRows。
func (r *UserRepository) Update(ctx context.Context, id int64, name *string, passwordHash *string) (*model.User, error) {
	sets := make([]string, 0, 2)
	args := make([]any, 0, 3)
	args = append(args, id)
	if name != nil {
		args = append(args, *name)
		sets = append(sets, fmt.Sprintf("name=$%d", len(args)))
	}
	if passwordHash != nil {
		args = append(args, *passwordHash)
		sets = append(sets, fmt.Sprintf("password_hash=$%d", len(args)))
	}
	var u model.User
	err := r.pool.QueryRow(ctx,
		"UPDATE users SET "+strings.Join(sets, ", ")+", updated_at=now() WHERE id=$1 RETURNING "+userCols,
		args...,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Name, &u.Status, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &u, nil
}

// Delete 删除指定 id 的用户。vms 表仍存在 user_id 引用该用户时，外键
// 约束（SQLSTATE 23503，migration 0010 未指定 ON DELETE 子句，行为为
// NO ACTION）经 classifyDBError 映射为 ErrInUse——对应设计 D6 的
// "有资源禁删"。id 不存在时返回 pgx.ErrNoRows。
func (r *UserRepository) Delete(ctx context.Context, id int64) error {
	tag, err := r.pool.Exec(ctx, "DELETE FROM users WHERE id=$1", id)
	if err != nil {
		return classifyDBError(err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// SetStatus 更新用户的启用状态（enabled/disabled，取值校验位于 service
// 层），返回更新后的行；id 不存在时返回 pgx.ErrNoRows。
func (r *UserRepository) SetStatus(ctx context.Context, id int64, status string) (*model.User, error) {
	var u model.User
	err := r.pool.QueryRow(ctx,
		"UPDATE users SET status=$1, updated_at=now() WHERE id=$2 RETURNING "+userCols,
		status, id,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Name, &u.Status, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &u, nil
}
