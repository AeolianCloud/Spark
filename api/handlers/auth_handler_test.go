package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"spark/api/middleware"
	"spark/model"
	"spark/service"
)

// testJWTSecret 是 handler 测试共用的固定 JWT 密钥（≥32 字符）。
const testJWTSecret = "test-jwt-secret-0123456789abcdefghijklmnopqrstuv"

// fakeAuthRepo 是供测试使用的可脚本化 service.AuthRepository。
type fakeAuthRepo struct {
	admins []model.Admin
	users  []model.User
	err    error
}

func (f *fakeAuthRepo) GetAdminByUsername(_ context.Context, username string) (*model.Admin, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.admins {
		if f.admins[i].Username == username {
			a := f.admins[i]
			return &a, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (f *fakeAuthRepo) GetUserByUsername(_ context.Context, username string) (*model.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.users {
		if f.users[i].Username == username {
			u := f.users[i]
			return &u, nil
		}
	}
	return nil, pgx.ErrNoRows
}

// newAuthTestRouter 构建只挂载双登录路由的测试引擎。
func newAuthTestRouter(t *testing.T, repo *fakeAuthRepo) *gin.Engine {
	t.Helper()
	svc, err := service.NewAuthService([]byte(testJWTSecret), repo)
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAuthRoutes(r.Group("/auth"), svc)
	return r
}

// postLogin 发送登录请求并返回 recorder。
func postLogin(t *testing.T, r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

// mustHashTest 生成 bcrypt 哈希，失败即终止测试。
func mustHashTest(t *testing.T, password string) string {
	t.Helper()
	hash, err := service.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return hash
}

// assertAuthError 断言 401 响应：统一错误契约、x-ms-error-code 头与
// 脱敏消息。
func assertAuthError(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if got := w.Header().Get(middleware.XMSErrorCodeHeader); got != CodeUnauthorized {
		t.Errorf("x-ms-error-code = %q, want %q", got, CodeUnauthorized)
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != CodeUnauthorized {
		t.Errorf("body error code = %q, want %q", body.Error.Code, CodeUnauthorized)
	}
	if body.Error.Message != "invalid credentials" {
		t.Errorf("body error message = %q, want unified %q (不泄露具体原因)",
			body.Error.Message, "invalid credentials")
	}
}

func TestUserLoginSuccess(t *testing.T) {
	hash := mustHashTest(t, "pw-123")
	repo := &fakeAuthRepo{users: []model.User{
		{ID: 2, Username: "alice", PasswordHash: hash, Name: "Alice", Status: model.UserStatusEnabled},
	}}
	r := newAuthTestRouter(t, repo)

	w := postLogin(t, r, "/auth/login", `{"username":"alice","password":"pw-123"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var body struct {
		Token    string `json:"token"`
		UserID   int64  `json:"user_id"`
		Username string `json:"username"`
		Name     string `json:"name"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Token == "" || body.UserID != 2 || body.Username != "alice" || body.Name != "Alice" {
		t.Fatalf("body = %+v, want token + user 2 alice/Alice", body)
	}
}

func TestUserLoginWrongPassword(t *testing.T) {
	hash := mustHashTest(t, "pw-123")
	repo := &fakeAuthRepo{users: []model.User{
		{ID: 2, Username: "alice", PasswordHash: hash, Name: "Alice", Status: model.UserStatusEnabled},
	}}
	r := newAuthTestRouter(t, repo)

	assertAuthError(t, postLogin(t, r, "/auth/login", `{"username":"alice","password":"wrong"}`))
}

func TestUserLoginDisabled(t *testing.T) {
	// 禁用用户：密码正确也 401，消息与凭证无效一致（任务 3.4）。
	hash := mustHashTest(t, "pw-123")
	repo := &fakeAuthRepo{users: []model.User{
		{ID: 2, Username: "alice", PasswordHash: hash, Name: "Alice", Status: model.UserStatusDisabled},
	}}
	r := newAuthTestRouter(t, repo)

	assertAuthError(t, postLogin(t, r, "/auth/login", `{"username":"alice","password":"pw-123"}`))
}

func TestUserLoginUnknownAccount(t *testing.T) {
	// 账号不存在与密码错误响应一致（不泄露账号存在性）。
	r := newAuthTestRouter(t, &fakeAuthRepo{})
	assertAuthError(t, postLogin(t, r, "/auth/login", `{"username":"nobody","password":"pw-123"}`))
}

func TestUserLoginBadBody(t *testing.T) {
	// 非法 JSON（空体、语法错误、字段类型不匹配）→ 400；缺字段本身
	// 不是绑定错误（gin 无 required 标签），由 service 层按凭证无效处理。
	r := newAuthTestRouter(t, &fakeAuthRepo{})
	cases := []string{"", "{", `{"username":123,"password":456}`}
	for _, body := range cases {
		w := postLogin(t, r, "/auth/login", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, w.Code)
		}
		if got := w.Header().Get(middleware.XMSErrorCodeHeader); got != CodeBadRequest {
			t.Errorf("body %q: x-ms-error-code = %q, want %q", body, got, CodeBadRequest)
		}
	}
}

func TestAdminLoginSuccess(t *testing.T) {
	hash := mustHashTest(t, "admin-pw")
	repo := &fakeAuthRepo{admins: []model.Admin{
		{ID: 1, Username: "root", PasswordHash: hash},
	}}
	r := newAuthTestRouter(t, repo)

	w := postLogin(t, r, "/auth/admin/login", `{"username":"root","password":"admin-pw"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var body struct {
		Token    string `json:"token"`
		AdminID  int64  `json:"admin_id"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Token == "" || body.AdminID != 1 || body.Username != "root" {
		t.Fatalf("body = %+v, want token + admin 1 root", body)
	}
}

func TestAdminLoginWrongPassword(t *testing.T) {
	hash := mustHashTest(t, "admin-pw")
	repo := &fakeAuthRepo{admins: []model.Admin{
		{ID: 1, Username: "root", PasswordHash: hash},
	}}
	r := newAuthTestRouter(t, repo)

	assertAuthError(t, postLogin(t, r, "/auth/admin/login", `{"username":"root","password":"wrong"}`))
}

// TestLoginMessageIsSanitized 锁定登录错误消息不含用户名/密码等输入回显
// （防日志注入与账号枚举）。
func TestLoginMessageIsSanitized(t *testing.T) {
	r := newAuthTestRouter(t, &fakeAuthRepo{})
	w := postLogin(t, r, "/auth/login", `{"username":"alice\nx","password":"pw"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "alice") || strings.Contains(body, "x") {
		t.Fatalf("login error body leaks input: %s", body)
	}
}
