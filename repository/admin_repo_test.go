package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestAdminCreate 验证创建管理员：写入 username/password_hash 并返回
// 完整行（含 id 与时间戳）。password_hash 由调用方（CLI）传入 bcrypt
// 哈希，repository 层不接触明文。
func TestAdminCreate(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("INSERT INTO admins (username, password_hash) VALUES ($1, $2) RETURNING id, username, password_hash, created_at, updated_at").
		WithArgs("root", "$2a$10$hash").
		WillReturnRows(adminAuthRows(t).
			AddRow(int64(1), "root", "$2a$10$hash", testTime, testTime))

	repo := NewAdminRepository(mock)
	admin, err := repo.CreateAdmin(context.Background(), "root", "$2a$10$hash")
	if err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}
	if admin.ID != 1 || admin.Username != "root" || admin.PasswordHash != "$2a$10$hash" {
		t.Fatalf("admin = %+v, want id 1/root with password_hash", admin)
	}
	if !admin.CreatedAt.Equal(testTime) || !admin.UpdatedAt.Equal(testTime) {
		t.Fatalf("timestamps = %v/%v, want %v", admin.CreatedAt, admin.UpdatedAt, testTime)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestAdminCreateDuplicateUsername 验证 username 唯一约束冲突（SQLSTATE
// 23505）映射为 ErrConflict，CLI 据此输出 "admin already exists" 提示。
func TestAdminCreateDuplicateUsername(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("INSERT INTO admins (username, password_hash) VALUES ($1, $2) RETURNING id, username, password_hash, created_at, updated_at").
		WithArgs("root", "$2a$10$hash").
		WillReturnError(&pgconn.PgError{Code: "23505"})

	repo := NewAdminRepository(mock)
	_, err := repo.CreateAdmin(context.Background(), "root", "$2a$10$hash")
	if err != ErrConflict {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestAdminCreateOtherError 验证非唯一约束错误（如数据库不可用）原样
// 返回，由 CLI 包裹上下文后退出非零码。
func TestAdminCreateOtherError(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("INSERT INTO admins (username, password_hash) VALUES ($1, $2) RETURNING id, username, password_hash, created_at, updated_at").
		WithArgs("root", "$2a$10$hash").
		WillReturnError(errors.New("connection refused"))

	repo := NewAdminRepository(mock)
	_, err := repo.CreateAdmin(context.Background(), "root", "$2a$10$hash")
	if err == nil || errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want non-conflict error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
