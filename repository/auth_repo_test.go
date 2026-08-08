package repository

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

// adminAuthRows 是 admins 表查询结果的行构造助手（adminCols 列序）。
func adminAuthRows(t *testing.T) *pgxmock.Rows {
	t.Helper()
	return pgxmock.NewRows([]string{"id", "username", "password_hash", "created_at", "updated_at"})
}

// userAuthRows 是 users 表查询结果的行构造助手（userCols 列序）。
func userAuthRows(t *testing.T) *pgxmock.Rows {
	t.Helper()
	return pgxmock.NewRows([]string{"id", "username", "password_hash", "name", "status", "created_at", "updated_at"})
}

// TestGetAdminByUsername 验证按用户名查询管理员（含 password_hash 列），
// 且返回行携带登录校验所需全部字段。
func TestGetAdminByUsername(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT id, username, password_hash, created_at, updated_at FROM admins WHERE username=$1").
		WithArgs("root").
		WillReturnRows(adminAuthRows(t).
			AddRow(int64(1), "root", "$2a$10$hash", testTime, testTime))

	repo := NewAuthRepository(mock)
	admin, err := repo.GetAdminByUsername(context.Background(), "root")
	if err != nil {
		t.Fatalf("GetAdminByUsername: %v", err)
	}
	if admin.ID != 1 || admin.Username != "root" || admin.PasswordHash != "$2a$10$hash" {
		t.Fatalf("admin = %+v, want id 1/root with password_hash", admin)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestGetAdminByUsernameNoRows 验证账号不存在返回 pgx.ErrNoRows（service
// 层据此统一映射为 unauthorized，不区分账号存在性）。
func TestGetAdminByUsernameNoRows(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT id, username, password_hash, created_at, updated_at FROM admins WHERE username=$1").
		WithArgs("nobody").
		WillReturnError(pgx.ErrNoRows)

	repo := NewAuthRepository(mock)
	_, err := repo.GetAdminByUsername(context.Background(), "nobody")
	if err != pgx.ErrNoRows {
		t.Fatalf("err = %v, want pgx.ErrNoRows", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestGetAdminByID 验证按 id 查询管理员（鉴权中间件校验账号仍存在）。
func TestGetAdminByID(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT id, username, password_hash, created_at, updated_at FROM admins WHERE id=$1").
		WithArgs(int64(3)).
		WillReturnRows(adminAuthRows(t).
			AddRow(int64(3), "root", "$2a$10$hash", testTime, testTime))

	repo := NewAuthRepository(mock)
	admin, err := repo.GetAdminByID(context.Background(), 3)
	if err != nil {
		t.Fatalf("GetAdminByID: %v", err)
	}
	if admin.ID != 3 || admin.Username != "root" {
		t.Fatalf("admin = %+v, want id 3/root", admin)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestGetAdminByIDNoRows 验证 id 不存在返回 pgx.ErrNoRows（中间件据此
// 返回 401）。
func TestGetAdminByIDNoRows(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT id, username, password_hash, created_at, updated_at FROM admins WHERE id=$1").
		WithArgs(int64(99)).
		WillReturnError(pgx.ErrNoRows)

	repo := NewAuthRepository(mock)
	_, err := repo.GetAdminByID(context.Background(), 99)
	if err != pgx.ErrNoRows {
		t.Fatalf("err = %v, want pgx.ErrNoRows", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestGetUserByUsername 验证按用户名查询用户（含 password_hash、name 与
// status 列），返回行携带登录与禁用校验所需全部字段。
func TestGetUserByUsername(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT id, username, password_hash, name, status, created_at, updated_at FROM users WHERE username=$1").
		WithArgs("alice").
		WillReturnRows(userAuthRows(t).
			AddRow(int64(2), "alice", "$2a$10$hash", "Alice", "enabled", testTime, testTime))

	repo := NewAuthRepository(mock)
	user, err := repo.GetUserByUsername(context.Background(), "alice")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if user.ID != 2 || user.Username != "alice" || user.PasswordHash != "$2a$10$hash" ||
		user.Name != "Alice" || user.Status != "enabled" {
		t.Fatalf("user = %+v, want id 2/alice/enabled with password_hash and name", user)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestGetUserByUsernameNoRows 验证账号不存在返回 pgx.ErrNoRows（service
// 层据此统一映射为 unauthorized，与密码错误不加区分）。
func TestGetUserByUsernameNoRows(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT id, username, password_hash, name, status, created_at, updated_at FROM users WHERE username=$1").
		WithArgs("nobody").
		WillReturnError(pgx.ErrNoRows)

	repo := NewAuthRepository(mock)
	_, err := repo.GetUserByUsername(context.Background(), "nobody")
	if err != pgx.ErrNoRows {
		t.Fatalf("err = %v, want pgx.ErrNoRows", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestGetUserByID 验证按 id 查询用户（中间件校验账号存在且启用）——
// status 列随行返回，禁用判断由中间件基于该值完成。
func TestGetUserByID(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT id, username, password_hash, name, status, created_at, updated_at FROM users WHERE id=$1").
		WithArgs(int64(2)).
		WillReturnRows(userAuthRows(t).
			AddRow(int64(2), "alice", "$2a$10$hash", "Alice", "enabled", testTime, testTime))

	repo := NewAuthRepository(mock)
	user, err := repo.GetUserByID(context.Background(), 2)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if user.ID != 2 || user.Status != "enabled" {
		t.Fatalf("user = %+v, want id 2/enabled", user)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestGetUserByIDNoRows 验证 id 不存在返回 pgx.ErrNoRows（中间件据此
// 返回 401，不泄露账号是否存在）。
func TestGetUserByIDNoRows(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT id, username, password_hash, name, status, created_at, updated_at FROM users WHERE id=$1").
		WithArgs(int64(99)).
		WillReturnError(pgx.ErrNoRows)

	repo := NewAuthRepository(mock)
	_, err := repo.GetUserByID(context.Background(), 99)
	if err != pgx.ErrNoRows {
		t.Fatalf("err = %v, want pgx.ErrNoRows", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
