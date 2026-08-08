package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark/model"
	"spark/repository"
)

// fakeUserRepo 是供测试使用的可脚本化 UserRepository。
type fakeUserRepo struct {
	users    []model.User
	nextID   int64
	err      error
	conflict bool
	inUse    bool
}

func (f *fakeUserRepo) Create(_ context.Context, username, passwordHash, name string) (*model.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.conflict {
		return nil, repository.ErrConflict
	}
	u := model.User{
		ID: f.nextID, Username: username, PasswordHash: passwordHash,
		Name: name, Status: model.UserStatusEnabled,
		CreatedAt: testUserTime, UpdatedAt: testUserTime,
	}
	f.nextID++
	f.users = append(f.users, u)
	return &u, nil
}

func (f *fakeUserRepo) List(_ context.Context, limit, offset int) ([]model.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	if offset >= len(f.users) {
		return []model.User{}, nil
	}
	end := offset + limit
	if end > len(f.users) {
		end = len(f.users)
	}
	return f.users[offset:end], nil
}

func (f *fakeUserRepo) Count(_ context.Context) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	return len(f.users), nil
}

func (f *fakeUserRepo) GetByID(_ context.Context, id int64) (*model.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.users {
		if f.users[i].ID == id {
			u := f.users[i]
			return &u, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (f *fakeUserRepo) Update(_ context.Context, id int64, name, passwordHash *string) (*model.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.users {
		if f.users[i].ID == id {
			if name != nil {
				f.users[i].Name = *name
			}
			if passwordHash != nil {
				f.users[i].PasswordHash = *passwordHash
			}
			f.users[i].UpdatedAt = f.users[i].UpdatedAt.Add(time.Second)
			u := f.users[i]
			return &u, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (f *fakeUserRepo) Delete(_ context.Context, id int64) error {
	if f.err != nil {
		return f.err
	}
	if f.inUse {
		return repository.ErrInUse
	}
	for i := range f.users {
		if f.users[i].ID == id {
			f.users = append(f.users[:i], f.users[i+1:]...)
			return nil
		}
	}
	return pgx.ErrNoRows
}

func (f *fakeUserRepo) SetStatus(_ context.Context, id int64, status string) (*model.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.users {
		if f.users[i].ID == id {
			f.users[i].Status = status
			f.users[i].UpdatedAt = f.users[i].UpdatedAt.Add(time.Second)
			u := f.users[i]
			return &u, nil
		}
	}
	return nil, pgx.ErrNoRows
}

// testUserTime 是 fake 用户行的固定时间戳。
var testUserTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// seedUser 在 fake 仓储中预置一个用户，返回其 id。
func seedUser(f *fakeUserRepo, username, hash, name, status string) int64 {
	u := model.User{
		ID: f.nextID, Username: username, PasswordHash: hash,
		Name: name, Status: status,
		CreatedAt: testUserTime, UpdatedAt: testUserTime,
	}
	f.nextID++
	f.users = append(f.users, u)
	return u.ID
}

// newTestUserService 构建由 fake 仓储支撑的 UserService。
func newTestUserService(f *fakeUserRepo) *UserService {
	return NewUserService(f)
}

// assertServiceErrorKind 断言 err 是携带指定 kind 的 *service.Error，
// 定义于 image_service_test.go（同包共享），此处直接复用。

func TestCreateUserSuccess(t *testing.T) {
	// 合法请求：username 首尾空白被裁剪，密码经 bcrypt 哈希落库（可
	// 校验、不含明文），name 缺省为空字符串，status 默认 enabled。
	f := &fakeUserRepo{nextID: 2}
	svc := newTestUserService(f)

	user, err := svc.CreateUser(context.Background(), "  alice  ", "pw-123", "  Alice  ")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if user.ID != 2 || user.Username != "alice" || user.Name != "Alice" ||
		user.Status != model.UserStatusEnabled {
		t.Fatalf("user = %+v, want alice/Alice/enabled", user)
	}
	if user.PasswordHash == "pw-123" || strings.Contains(user.PasswordHash, "pw-123") {
		t.Fatal("password_hash must not contain the plaintext password")
	}
	if !VerifyPassword(user.PasswordHash, "pw-123") {
		t.Error("password_hash must verify against the original password")
	}
}

func TestCreateUserEmptyUsername(t *testing.T) {
	// username 必填：缺失、纯空白均被拒绝（400）。
	svc := newTestUserService(&fakeUserRepo{})
	for _, name := range []string{"", "   "} {
		_, err := svc.CreateUser(context.Background(), name, "pw-123", "")
		assertServiceErrorKind(t, err, KindBadRequest)
	}
}

func TestCreateUserUsernameTooLong(t *testing.T) {
	// username 超过契约 maxLength（128 字符）被拒绝（400）。
	svc := newTestUserService(&fakeUserRepo{})
	_, err := svc.CreateUser(context.Background(), strings.Repeat("名", maxUsernameLen+1), "pw-123", "")
	assertServiceErrorKind(t, err, KindBadRequest)
}

func TestCreateUserUsernameAtLimit(t *testing.T) {
	// 恰好 128 个多字节字符（UTF-8 按 rune 计数而非字节）可以通过。
	f := &fakeUserRepo{nextID: 1}
	svc := newTestUserService(f)
	user, err := svc.CreateUser(context.Background(), strings.Repeat("名", maxUsernameLen), "pw-123", "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if user.Username != strings.Repeat("名", maxUsernameLen) {
		t.Fatalf("username = %q, want the 128-rune name", user.Username)
	}
}

func TestCreateUserEmptyPassword(t *testing.T) {
	// password 必填：缺失被拒绝（400）。
	svc := newTestUserService(&fakeUserRepo{})
	_, err := svc.CreateUser(context.Background(), "alice", "", "")
	assertServiceErrorKind(t, err, KindBadRequest)
}

func TestCreateUserPasswordTooLong(t *testing.T) {
	// 密码超过 bcrypt 72 字节上限被拒绝（400），避免落为内部错误。
	svc := newTestUserService(&fakeUserRepo{})
	_, err := svc.CreateUser(context.Background(), "alice", strings.Repeat("a", maxPasswordBytes+1), "")
	assertServiceErrorKind(t, err, KindBadRequest)
}

func TestCreateUserPasswordAtLimit(t *testing.T) {
	// 恰好 72 字节的密码可以通过（bcrypt 上限边界）。
	f := &fakeUserRepo{nextID: 1}
	svc := newTestUserService(f)
	pw := strings.Repeat("a", maxPasswordBytes)
	user, err := svc.CreateUser(context.Background(), "alice", pw, "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if !VerifyPassword(user.PasswordHash, pw) {
		t.Error("password_hash must verify against the 72-byte password")
	}
}

func TestCreateUserDuplicateUsername(t *testing.T) {
	// username 重复（唯一约束冲突）→ 409 冲突，消息携带用户名。
	f := &fakeUserRepo{conflict: true}
	svc := newTestUserService(f)
	_, err := svc.CreateUser(context.Background(), "alice", "pw-123", "")
	assertServiceErrorKind(t, err, KindConflict)
}

func TestCreateUserRepoError(t *testing.T) {
	// 仓储故障是内部错误（500 语义），不是业务错误。
	f := &fakeUserRepo{err: errors.New("db down")}
	svc := newTestUserService(f)
	_, err := svc.CreateUser(context.Background(), "alice", "pw-123", "")
	var serr *Error
	if errors.As(err, &serr) {
		t.Fatalf("err = %v, want non-service error (kind %v)", err, serr.Kind)
	}
}

func TestListUsers(t *testing.T) {
	// 列表返回按序切片与总数（支撑 X-Total-Count）。
	f := &fakeUserRepo{nextID: 1}
	seedUser(f, "alice", "$2a$10$h1", "Alice", model.UserStatusEnabled)
	seedUser(f, "bob", "$2a$10$h2", "Bob", model.UserStatusDisabled)
	svc := newTestUserService(f)

	users, total, err := svc.ListUsers(context.Background(), 1, 1)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if total != 2 {
		t.Fatalf("total = %d, want 2", total)
	}
	if len(users) != 1 || users[0].Username != "bob" {
		t.Fatalf("users = %+v, want page with bob", users)
	}
}

func TestListUsersRepoError(t *testing.T) {
	f := &fakeUserRepo{err: errors.New("db down")}
	svc := newTestUserService(f)
	_, _, err := svc.ListUsers(context.Background(), 25, 0)
	var serr *Error
	if errors.As(err, &serr) {
		t.Fatalf("err = %v, want non-service error", err)
	}
}

func TestGetUserSuccess(t *testing.T) {
	f := &fakeUserRepo{nextID: 1}
	id := seedUser(f, "alice", "$2a$10$h", "Alice", model.UserStatusEnabled)
	svc := newTestUserService(f)

	user, err := svc.GetUser(context.Background(), id)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if user.Username != "alice" || user.Status != model.UserStatusEnabled {
		t.Fatalf("user = %+v, want alice/enabled", user)
	}
}

func TestGetUserNotFound(t *testing.T) {
	// 用户不存在 → 404。
	svc := newTestUserService(&fakeUserRepo{})
	_, err := svc.GetUser(context.Background(), 99)
	assertServiceErrorKind(t, err, KindNotFound)
}

func TestUpdateUserNameOnly(t *testing.T) {
	// 仅更新 name：密码哈希保持不变。
	f := &fakeUserRepo{nextID: 1}
	id := seedUser(f, "alice", "$2a$10$h", "Alice", model.UserStatusEnabled)
	svc := newTestUserService(f)

	name := "Alice2"
	user, err := svc.UpdateUser(context.Background(), id, &name, nil)
	if err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if user.Name != "Alice2" || user.PasswordHash != "$2a$10$h" {
		t.Fatalf("user = %+v, want name Alice2 with unchanged hash", user)
	}
}

func TestUpdateUserPasswordOnly(t *testing.T) {
	// 仅重置密码：新密码经 bcrypt 哈希落库，name 保持不变。
	f := &fakeUserRepo{nextID: 1}
	id := seedUser(f, "alice", "$2a$10$old", "Alice", model.UserStatusEnabled)
	svc := newTestUserService(f)

	pw := "new-pw"
	user, err := svc.UpdateUser(context.Background(), id, nil, &pw)
	if err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if user.Name != "Alice" {
		t.Fatalf("name = %q, want unchanged Alice", user.Name)
	}
	if user.PasswordHash == "$2a$10$old" || !VerifyPassword(user.PasswordHash, pw) {
		t.Fatalf("password_hash = %q, want bcrypt hash of new password", user.PasswordHash)
	}
}

func TestUpdateUserBoth(t *testing.T) {
	f := &fakeUserRepo{nextID: 1}
	id := seedUser(f, "alice", "$2a$10$old", "Alice", model.UserStatusEnabled)
	svc := newTestUserService(f)

	name, pw := "Alice2", "new-pw"
	user, err := svc.UpdateUser(context.Background(), id, &name, &pw)
	if err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if user.Name != "Alice2" || !VerifyPassword(user.PasswordHash, pw) {
		t.Fatalf("user = %+v, want name Alice2 with new password hash", user)
	}
}

func TestUpdateUserNoFields(t *testing.T) {
	// name 与 password 都缺省 → 400（至少提供一个）。
	svc := newTestUserService(&fakeUserRepo{})
	_, err := svc.UpdateUser(context.Background(), 1, nil, nil)
	assertServiceErrorKind(t, err, KindBadRequest)
}

func TestUpdateUserPasswordTooLong(t *testing.T) {
	// 重置密码同样受 72 字节上限约束。
	f := &fakeUserRepo{nextID: 1}
	id := seedUser(f, "alice", "$2a$10$old", "Alice", model.UserStatusEnabled)
	svc := newTestUserService(f)

	pw := strings.Repeat("a", maxPasswordBytes+1)
	_, err := svc.UpdateUser(context.Background(), id, nil, &pw)
	assertServiceErrorKind(t, err, KindBadRequest)
}

func TestUpdateUserEmptyPassword(t *testing.T) {
	// 重置密码为空串被拒绝（400，S3）：空串会生成合法 bcrypt 哈希，等价
	// "无密码登录"后门，与 CreateUser 的必填校验对齐。
	f := &fakeUserRepo{nextID: 1}
	id := seedUser(f, "alice", "$2a$10$old", "Alice", model.UserStatusEnabled)
	svc := newTestUserService(f)

	pw := ""
	_, err := svc.UpdateUser(context.Background(), id, nil, &pw)
	assertServiceErrorKind(t, err, KindBadRequest)
	if len(f.users) != 1 || f.users[0].PasswordHash != "$2a$10$old" {
		t.Fatalf("password must not be overwritten by an empty string, users = %+v", f.users)
	}
}

func TestUpdateUserNotFound(t *testing.T) {
	// 更新不存在的用户 → 404。
	f := &fakeUserRepo{nextID: 1}
	seedUser(f, "alice", "$2a$10$h", "Alice", model.UserStatusEnabled)
	svc := newTestUserService(f)

	name := "Alice2"
	_, err := svc.UpdateUser(context.Background(), 99, &name, nil)
	assertServiceErrorKind(t, err, KindNotFound)
}

func TestDeleteUserSuccess(t *testing.T) {
	// 无关联资源的用户删除成功。
	f := &fakeUserRepo{nextID: 1}
	id := seedUser(f, "alice", "$2a$10$h", "Alice", model.UserStatusEnabled)
	svc := newTestUserService(f)

	if err := svc.DeleteUser(context.Background(), id); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if len(f.users) != 0 {
		t.Fatalf("users = %+v, want empty after delete", f.users)
	}
}

func TestDeleteUserInUse(t *testing.T) {
	// 名下仍关联虚拟机（外键引用）→ user_has_resources 冲突错误。
	f := &fakeUserRepo{nextID: 1, inUse: true}
	svc := newTestUserService(f)

	err := svc.DeleteUser(context.Background(), 1)
	assertServiceErrorKind(t, err, KindUserHasResources)
}

func TestDeleteUserNotFound(t *testing.T) {
	// 删除不存在的用户 → 404。
	svc := newTestUserService(&fakeUserRepo{})
	err := svc.DeleteUser(context.Background(), 99)
	assertServiceErrorKind(t, err, KindNotFound)
}

func TestSetUserStatusSuccess(t *testing.T) {
	// 启用/禁用切换返回更新后的用户。
	f := &fakeUserRepo{nextID: 1}
	id := seedUser(f, "alice", "$2a$10$h", "Alice", model.UserStatusEnabled)
	svc := newTestUserService(f)

	user, err := svc.SetUserStatus(context.Background(), id, model.UserStatusDisabled)
	if err != nil {
		t.Fatalf("SetUserStatus: %v", err)
	}
	if user.Status != model.UserStatusDisabled {
		t.Fatalf("status = %q, want disabled", user.Status)
	}

	user, err = svc.SetUserStatus(context.Background(), id, model.UserStatusEnabled)
	if err != nil {
		t.Fatalf("SetUserStatus(enable): %v", err)
	}
	if user.Status != model.UserStatusEnabled {
		t.Fatalf("status = %q, want enabled", user.Status)
	}
}

func TestSetUserStatusInvalid(t *testing.T) {
	// 非法状态取值（枚举校验）→ 400。
	f := &fakeUserRepo{nextID: 1}
	id := seedUser(f, "alice", "$2a$10$h", "Alice", model.UserStatusEnabled)
	svc := newTestUserService(f)

	for _, status := range []string{"", "active", "ENABLED", "suspended"} {
		_, err := svc.SetUserStatus(context.Background(), id, status)
		assertServiceErrorKind(t, err, KindBadRequest)
	}
}

func TestSetUserStatusNotFound(t *testing.T) {
	// 状态切换不存在的用户 → 404。
	svc := newTestUserService(&fakeUserRepo{})
	_, err := svc.SetUserStatus(context.Background(), 99, model.UserStatusDisabled)
	assertServiceErrorKind(t, err, KindNotFound)
}
