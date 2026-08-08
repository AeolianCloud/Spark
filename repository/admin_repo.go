package repository

import (
	"context"

	"spark/model"
)

// AdminRepository 提供 admins 表的写操作（设计 D7：种子管理员 CLI）。
// 登录/鉴权所需的只读查询（GetAdminByUsername/GetAdminByID）由
// AuthRepository 承担（见 auth_repo.go 的职责注释），本仓储只负责
// 创建，两者互不干扰。
type AdminRepository struct {
	pool pgxQuerier
}

// NewAdminRepository 创建由 pool 支撑的 AdminRepository。
func NewAdminRepository(pool pgxQuerier) *AdminRepository {
	return &AdminRepository{pool: pool}
}

// CreateAdmin 插入一个管理员并返回它（已填充 id 与时间戳）。passwordHash
// 必须是调用方传入的 bcrypt 哈希——明文密码绝不进入 repository 层。
// username 重复时产生 ErrConflict（SQLSTATE 23505 经 classifyDBError
// 映射），由 CLI 映射为 "admin already exists" 提示。
func (r *AdminRepository) CreateAdmin(ctx context.Context, username, passwordHash string) (*model.Admin, error) {
	var a model.Admin
	err := r.pool.QueryRow(ctx,
		"INSERT INTO admins (username, password_hash) VALUES ($1, $2) RETURNING "+adminCols,
		username, passwordHash,
	).Scan(&a.ID, &a.Username, &a.PasswordHash, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &a, nil
}
