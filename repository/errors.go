package repository

import "errors"

// 仓库实现共享的哨兵错误。服务层会把这些（以及 pgx.ErrNoRows）
// 映射为 API 错误。
var (
	// ErrConflict 在插入或更新违反唯一约束时返回（PostgreSQL SQLSTATE 23505）。
	ErrConflict = errors.New("repository: unique constraint violation")
	// ErrInUse 在操作因其他行仍引用目标行而被拒绝时返回：删除（或
	// 插入/更新）违反了外键（PostgreSQL SQLSTATE 23503）。
	ErrInUse = errors.New("repository: resource still in use")
	// ErrSpecConflict 在乐观锁更新没有命中任何行时返回：规格在调用方
	// 读取与写入之间被并发修改（或行被删除）。
	ErrSpecConflict = errors.New("repository: vm spec was concurrently modified")
)
