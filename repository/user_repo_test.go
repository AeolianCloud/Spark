package repository

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

// userCols 列序与 auth_repo.go 的 userAuthRows helper 一致
// （id, username, password_hash, name, status, created_at, updated_at）。

// TestUserCreate 验证创建用户：写入 username/password_hash/name 并返回
// 完整行（含 id 与时间戳）。password_hash 由调用方（service 层）传入
// bcrypt 哈希，repository 层不接触明文。
func TestUserCreate(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("INSERT INTO users (username, password_hash, name) VALUES ($1, $2, $3) RETURNING id, username, password_hash, name, status, created_at, updated_at").
		WithArgs("alice", "$2a$10$hash", "Alice").
		WillReturnRows(userAuthRows(t).
			AddRow(int64(2), "alice", "$2a$10$hash", "Alice", "enabled", testTime, testTime))

	repo := NewUserRepository(mock)
	user, err := repo.Create(context.Background(), "alice", "$2a$10$hash", "Alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if user.ID != 2 || user.Username != "alice" || user.PasswordHash != "$2a$10$hash" ||
		user.Name != "Alice" || user.Status != "enabled" {
		t.Fatalf("user = %+v, want id 2/alice/enabled", user)
	}
	if !user.CreatedAt.Equal(testTime) || !user.UpdatedAt.Equal(testTime) {
		t.Fatalf("timestamps = %v/%v, want %v", user.CreatedAt, user.UpdatedAt, testTime)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUserCreateDuplicateUsername 验证 username 唯一约束冲突（SQLSTATE
// 23505）映射为 ErrConflict。
func TestUserCreateDuplicateUsername(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("INSERT INTO users (username, password_hash, name) VALUES ($1, $2, $3) RETURNING id, username, password_hash, name, status, created_at, updated_at").
		WithArgs("alice", "$2a$10$hash", "").
		WillReturnError(&pgconn.PgError{Code: "23505"})

	repo := NewUserRepository(mock)
	_, err := repo.Create(context.Background(), "alice", "$2a$10$hash", "")
	if err != ErrConflict {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUserListPage 验证分页列表：按 id 排序并应用 limit/offset。
func TestUserListPage(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT id, username, password_hash, name, status, created_at, updated_at FROM users ORDER BY id LIMIT $1 OFFSET $2").
		WithArgs(25, 0).
		WillReturnRows(userAuthRows(t).
			AddRow(int64(1), "alice", "$2a$10$h1", "Alice", "enabled", testTime, testTime).
			AddRow(int64(2), "bob", "$2a$10$h2", "Bob", "disabled", testTime.Add(1), testTime.Add(1)))

	repo := NewUserRepository(mock)
	users, err := repo.List(context.Background(), 25, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(users) != 2 || users[0].Username != "alice" || users[1].Username != "bob" ||
		users[1].Status != "disabled" {
		t.Fatalf("users = %+v, want alice(1)/bob(2)", users)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUserListEmptyPage 验证 offset 越界时返回空列表而非错误。
func TestUserListEmptyPage(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT id, username, password_hash, name, status, created_at, updated_at FROM users ORDER BY id LIMIT $1 OFFSET $2").
		WithArgs(25, 100).
		WillReturnRows(userAuthRows(t))

	repo := NewUserRepository(mock)
	users, err := repo.List(context.Background(), 25, 100)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(users) != 0 {
		t.Fatalf("users = %+v, want empty list", users)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUserCount 验证总数查询（支撑 X-Total-Count）。
func TestUserCount(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT count(*) FROM users").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(7)))

	repo := NewUserRepository(mock)
	total, err := repo.Count(context.Background())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 7 {
		t.Fatalf("total = %d, want 7", total)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUserGetByID 验证按 id 查询用户（含全部列）。
func TestUserGetByID(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT id, username, password_hash, name, status, created_at, updated_at FROM users WHERE id=$1").
		WithArgs(int64(2)).
		WillReturnRows(userAuthRows(t).
			AddRow(int64(2), "alice", "$2a$10$hash", "Alice", "enabled", testTime, testTime))

	repo := NewUserRepository(mock)
	user, err := repo.GetByID(context.Background(), 2)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if user.ID != 2 || user.Username != "alice" {
		t.Fatalf("user = %+v, want id 2/alice", user)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUserGetByIDNoRows 验证 id 不存在返回 pgx.ErrNoRows。
func TestUserGetByIDNoRows(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT id, username, password_hash, name, status, created_at, updated_at FROM users WHERE id=$1").
		WithArgs(int64(99)).
		WillReturnError(pgx.ErrNoRows)

	repo := NewUserRepository(mock)
	_, err := repo.GetByID(context.Background(), 99)
	if err != pgx.ErrNoRows {
		t.Fatalf("err = %v, want pgx.ErrNoRows", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUserUpdateName 验证仅更新 name（password_hash 保持原值）。
func TestUserUpdateName(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("UPDATE users SET name=$2, updated_at=now() WHERE id=$1 RETURNING id, username, password_hash, name, status, created_at, updated_at").
		WithArgs(int64(2), "Alice2").
		WillReturnRows(userAuthRows(t).
			AddRow(int64(2), "alice", "$2a$10$hash", "Alice2", "enabled", testTime, testTime.Add(1)))

	repo := NewUserRepository(mock)
	user, err := repo.Update(context.Background(), 2, strPtr("Alice2"), nil)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if user.Name != "Alice2" || user.PasswordHash != "$2a$10$hash" {
		t.Fatalf("user = %+v, want name Alice2 with unchanged hash", user)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUserUpdatePassword 验证仅重置密码（name 保持原值）。
func TestUserUpdatePassword(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("UPDATE users SET password_hash=$2, updated_at=now() WHERE id=$1 RETURNING id, username, password_hash, name, status, created_at, updated_at").
		WithArgs(int64(2), "$2a$10$newhash").
		WillReturnRows(userAuthRows(t).
			AddRow(int64(2), "alice", "$2a$10$newhash", "Alice", "enabled", testTime, testTime.Add(1)))

	repo := NewUserRepository(mock)
	user, err := repo.Update(context.Background(), 2, nil, strPtr("$2a$10$newhash"))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if user.PasswordHash != "$2a$10$newhash" || user.Name != "Alice" {
		t.Fatalf("user = %+v, want new hash with unchanged name", user)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUserUpdateBoth 验证同时更新 name 与 password_hash。
func TestUserUpdateBoth(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("UPDATE users SET name=$2, password_hash=$3, updated_at=now() WHERE id=$1 RETURNING id, username, password_hash, name, status, created_at, updated_at").
		WithArgs(int64(2), "Alice2", "$2a$10$newhash").
		WillReturnRows(userAuthRows(t).
			AddRow(int64(2), "alice", "$2a$10$newhash", "Alice2", "enabled", testTime, testTime.Add(1)))

	repo := NewUserRepository(mock)
	user, err := repo.Update(context.Background(), 2, strPtr("Alice2"), strPtr("$2a$10$newhash"))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if user.Name != "Alice2" || user.PasswordHash != "$2a$10$newhash" {
		t.Fatalf("user = %+v, want name Alice2 with new hash", user)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUserUpdateNoRows 验证更新不存在的用户返回 pgx.ErrNoRows。
func TestUserUpdateNoRows(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("UPDATE users SET name=$2, updated_at=now() WHERE id=$1 RETURNING id, username, password_hash, name, status, created_at, updated_at").
		WithArgs(int64(99), "Alice").
		WillReturnError(pgx.ErrNoRows)

	repo := NewUserRepository(mock)
	_, err := repo.Update(context.Background(), 99, strPtr("Alice"), nil)
	if err != pgx.ErrNoRows {
		t.Fatalf("err = %v, want pgx.ErrNoRows", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUserDelete 验证删除用户成功（RowsAffected=1）。
func TestUserDelete(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectExec("DELETE FROM users WHERE id=$1").
		WithArgs(int64(2)).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	repo := NewUserRepository(mock)
	if err := repo.Delete(context.Background(), 2); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUserDeleteInUse 验证名下仍有虚拟机引用时（外键约束 SQLSTATE
// 23503）映射为 ErrInUse（设计 D6 的"有资源禁删"）。
func TestUserDeleteInUse(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectExec("DELETE FROM users WHERE id=$1").
		WithArgs(int64(2)).
		WillReturnError(&pgconn.PgError{Code: "23503"})

	repo := NewUserRepository(mock)
	err := repo.Delete(context.Background(), 2)
	if err != ErrInUse {
		t.Fatalf("err = %v, want ErrInUse", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUserDeleteNoRows 验证删除不存在的用户返回 pgx.ErrNoRows。
func TestUserDeleteNoRows(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectExec("DELETE FROM users WHERE id=$1").
		WithArgs(int64(99)).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	repo := NewUserRepository(mock)
	err := repo.Delete(context.Background(), 99)
	if err != pgx.ErrNoRows {
		t.Fatalf("err = %v, want pgx.ErrNoRows", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUserSetStatus 验证状态切换（启用/禁用）并返回更新后的行。
func TestUserSetStatus(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("UPDATE users SET status=$1, updated_at=now() WHERE id=$2 RETURNING id, username, password_hash, name, status, created_at, updated_at").
		WithArgs("disabled", int64(2)).
		WillReturnRows(userAuthRows(t).
			AddRow(int64(2), "alice", "$2a$10$hash", "Alice", "disabled", testTime, testTime.Add(1)))

	repo := NewUserRepository(mock)
	user, err := repo.SetStatus(context.Background(), 2, "disabled")
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if user.Status != "disabled" {
		t.Fatalf("status = %q, want disabled", user.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUserSetStatusNoRows 验证状态切换不存在的用户返回 pgx.ErrNoRows。
func TestUserSetStatusNoRows(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("UPDATE users SET status=$1, updated_at=now() WHERE id=$2 RETURNING id, username, password_hash, name, status, created_at, updated_at").
		WithArgs("enabled", int64(99)).
		WillReturnError(pgx.ErrNoRows)

	repo := NewUserRepository(mock)
	_, err := repo.SetStatus(context.Background(), 99, "enabled")
	if err != pgx.ErrNoRows {
		t.Fatalf("err = %v, want pgx.ErrNoRows", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// strPtr 返回指向 s 的指针，供 Update 的可选字段参数使用。
func strPtr(s string) *string {
	return &s
}
