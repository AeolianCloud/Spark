package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"

	"spark/model"
)

// testJWTSecret 是 middleware 测试共用的固定 JWT 密钥（≥32 字符）。
const testJWTSecret = "test-jwt-secret-0123456789abcdefghijklmnopqrstuv"

// fakeCredentialRepo 是供测试使用的可脚本化 CredentialRepository。
type fakeCredentialRepo struct {
	admins []model.Admin
	users  []model.User
	err    error
}

func (f *fakeCredentialRepo) GetAdminByID(_ context.Context, id int64) (*model.Admin, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.admins {
		if f.admins[i].ID == id {
			a := f.admins[i]
			return &a, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (f *fakeCredentialRepo) GetUserByID(_ context.Context, id int64) (*model.User, error) {
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

// signTestToken 用固定密钥签发测试令牌；expiry 为 nil 时取 +24h。
// 复用生产 identityClaims 结构，保证测试令牌与鉴权解析的 claims 一致。
func signTestToken(t *testing.T, role string, id int64, expiry *time.Time) string {
	t.Helper()
	now := time.Now()
	exp := now.Add(24 * time.Hour)
	if expiry != nil {
		exp = *expiry
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, identityClaims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   fmt.Sprintf("%d", id),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	})
	signed, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	return signed
}

// newAuthTestRouter 构建挂载 requireAuth + requireAdmin 的测试引擎：
// GET /me 报告注入的身份（校验身份注入）；GET /admin 走 requireAdmin。
func newAuthTestRouter(t *testing.T, repo CredentialRepository) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/me", RequireAuth([]byte(testJWTSecret), repo), func(c *gin.Context) {
		ident := c.MustGet(IdentityKey).(Identity)
		c.JSON(http.StatusOK, gin.H{"role": ident.Role, "id": ident.ID})
	})
	r.GET("/admin", RequireAuth([]byte(testJWTSecret), repo), RequireAdmin(), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	return r
}

// authErrorBody 镜像统一的鉴权错误负载。
type authErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// assertAuthResponse 断言响应状态码、x-ms-error-code 头与 body 错误码一致。
func assertAuthResponse(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if w.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, wantStatus, w.Body.String())
	}
	if got := w.Header().Get(XMSErrorCodeHeader); got != wantCode {
		t.Errorf("x-ms-error-code = %q, want %q", got, wantCode)
	}
	var body authErrorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != wantCode {
		t.Errorf("body error code = %q, want %q", body.Error.Code, wantCode)
	}
	if body.Error.Code != w.Header().Get(XMSErrorCodeHeader) {
		t.Errorf("body error code = %q, want header value %q",
			body.Error.Code, w.Header().Get(XMSErrorCodeHeader))
	}
	if body.Error.Message == "" {
		t.Error("body error message is empty")
	}
}

// getWithToken 发起带/不带 Bearer 令牌的 GET 请求。
func getWithToken(t *testing.T, r *gin.Engine, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	r.ServeHTTP(w, req)
	return w
}

func TestRequireAuthMissingToken(t *testing.T) {
	// 缺失 Authorization 头或非 Bearer 格式一律 401（消息统一）。
	repo := &fakeCredentialRepo{users: []model.User{{ID: 2, Status: model.UserStatusEnabled}}}
	r := newAuthTestRouter(t, repo)

	cases := []struct {
		name   string
		header string
	}{
		{name: "no header"},
		{name: "bare token", header: "not-a-bearer-token"},
		{name: "basic auth", header: "Basic abc123"},
		{name: "empty bearer", header: "Bearer "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/me", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			r.ServeHTTP(w, req)
			assertAuthResponse(t, w, http.StatusUnauthorized, errCodeUnauthorized)
		})
	}
}

func TestRequireAuthInvalidToken(t *testing.T) {
	// 垃圾令牌与换密钥签发的令牌都 401。
	repo := &fakeCredentialRepo{users: []model.User{{ID: 2, Status: model.UserStatusEnabled}}}
	r := newAuthTestRouter(t, repo)

	cases := []string{
		"garbage",
		"a.b.c",
		"garbage.bearer.token",
	}
	other, err := jwt.NewWithClaims(jwt.SigningMethodHS256, identityClaims{
		Role: RoleUser,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "2",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
		},
	}).SignedString([]byte(strings.Repeat("y", 44)))
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	cases = append(cases, other)

	for _, tok := range cases {
		w := getWithToken(t, r, "/me", tok)
		assertAuthResponse(t, w, http.StatusUnauthorized, errCodeUnauthorized)
	}
}

func TestRequireAuthExpiredToken(t *testing.T) {
	// 过期令牌 401。
	repo := &fakeCredentialRepo{users: []model.User{{ID: 2, Status: model.UserStatusEnabled}}}
	r := newAuthTestRouter(t, repo)

	expired := signTestToken(t, RoleUser, 2, &[]time.Time{time.Now().Add(-time.Hour)}[0])
	w := getWithToken(t, r, "/me", expired)
	assertAuthResponse(t, w, http.StatusUnauthorized, errCodeUnauthorized)
}

func TestRequireAuthMissingExp(t *testing.T) {
	// 无 exp 声明的令牌一律 401（L1：强制 exp 存在）。
	repo := &fakeCredentialRepo{users: []model.User{{ID: 2, Status: model.UserStatusEnabled}}}
	r := newAuthTestRouter(t, repo)

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, identityClaims{
		Role: RoleUser,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:  "2",
			IssuedAt: jwt.NewNumericDate(time.Now()),
		},
	})
	signed, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	w := getWithToken(t, r, "/me", signed)
	assertAuthResponse(t, w, http.StatusUnauthorized, errCodeUnauthorized)
}

func TestRequireAuthNonPositiveSub(t *testing.T) {
	// sub 为 0/负数的令牌一律 401（L3：sub 必须是正整数 ID）。
	repo := &fakeCredentialRepo{users: []model.User{{ID: 2, Status: model.UserStatusEnabled}}}
	r := newAuthTestRouter(t, repo)

	for _, id := range []int64{0, -1} {
		w := getWithToken(t, r, "/me", signTestToken(t, RoleUser, id, nil))
		assertAuthResponse(t, w, http.StatusUnauthorized, errCodeUnauthorized)
	}
}

func TestRequireAuthNonHS256Algorithm(t *testing.T) {
	// 签名算法白名单仅 HS256：HS512 签发的令牌即使同一密钥也 401（L2）。
	repo := &fakeCredentialRepo{users: []model.User{{ID: 2, Status: model.UserStatusEnabled}}}
	r := newAuthTestRouter(t, repo)

	token := jwt.NewWithClaims(jwt.SigningMethodHS512, identityClaims{
		Role: RoleUser,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "2",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
		},
	})
	signed, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	w := getWithToken(t, r, "/me", signed)
	assertAuthResponse(t, w, http.StatusUnauthorized, errCodeUnauthorized)
}

func TestRequireAuthDisabledUser(t *testing.T) {
	// 禁用用户：即使令牌有效也 401（设计 D4/D5：每次请求查库校验）。
	repo := &fakeCredentialRepo{users: []model.User{{ID: 2, Status: model.UserStatusDisabled}}}
	r := newAuthTestRouter(t, repo)

	w := getWithToken(t, r, "/me", signTestToken(t, RoleUser, 2, nil))
	assertAuthResponse(t, w, http.StatusUnauthorized, errCodeUnauthorized)
}

func TestRequireAuthDeletedUser(t *testing.T) {
	// 账号已删除（查库 ErrNoRows）也 401。
	repo := &fakeCredentialRepo{}
	r := newAuthTestRouter(t, repo)

	w := getWithToken(t, r, "/me", signTestToken(t, RoleUser, 99, nil))
	assertAuthResponse(t, w, http.StatusUnauthorized, errCodeUnauthorized)
}

func TestRequireAuthDeletedAdmin(t *testing.T) {
	// 管理员账号被删除后，旧 admin 令牌立即失效（401）。
	repo := &fakeCredentialRepo{}
	r := newAuthTestRouter(t, repo)

	w := getWithToken(t, r, "/me", signTestToken(t, RoleAdmin, 99, nil))
	assertAuthResponse(t, w, http.StatusUnauthorized, errCodeUnauthorized)
}

func TestRequireAuthRepoErrorIsInternal(t *testing.T) {
	// 查库失败（非 ErrNoRows，如 DB 不可用）返回 500，不伪装成 401。
	repo := &fakeCredentialRepo{err: fmt.Errorf("db down")}
	r := newAuthTestRouter(t, repo)

	w := getWithToken(t, r, "/me", signTestToken(t, RoleUser, 2, nil))
	assertAuthResponse(t, w, http.StatusInternalServerError, errCodeInternal)
}

func TestRequireAuthInjectsIdentity(t *testing.T) {
	// 有效令牌：身份注入 gin.Context（role + ID），handler 可读取。
	repo := &fakeCredentialRepo{users: []model.User{{ID: 2, Status: model.UserStatusEnabled}}}
	r := newAuthTestRouter(t, repo)

	w := getWithToken(t, r, "/me", signTestToken(t, RoleUser, 2, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var body struct {
		Role string `json:"role"`
		ID   int64  `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Role != RoleUser || body.ID != 2 {
		t.Fatalf("identity = %+v, want user 2", body)
	}
}

func TestRequireAuthAdminIdentity(t *testing.T) {
	repo := &fakeCredentialRepo{admins: []model.Admin{{ID: 1}}}
	r := newAuthTestRouter(t, repo)

	w := getWithToken(t, r, "/me", signTestToken(t, RoleAdmin, 1, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var body struct {
		Role string `json:"role"`
		ID   int64  `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Role != RoleAdmin || body.ID != 1 {
		t.Fatalf("identity = %+v, want admin 1", body)
	}
}

func TestRequireAdminAllowsAdmin(t *testing.T) {
	repo := &fakeCredentialRepo{admins: []model.Admin{{ID: 1}}}
	r := newAuthTestRouter(t, repo)

	w := getWithToken(t, r, "/admin", signTestToken(t, RoleAdmin, 1, nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", w.Code, w.Body.String())
	}
}

func TestRequireAdminRejectsUser(t *testing.T) {
	// 用户令牌访问管理员接口：403 forbidden（任务 4.2）。
	repo := &fakeCredentialRepo{users: []model.User{{ID: 2, Status: model.UserStatusEnabled}}}
	r := newAuthTestRouter(t, repo)

	w := getWithToken(t, r, "/admin", signTestToken(t, RoleUser, 2, nil))
	assertAuthResponse(t, w, http.StatusForbidden, errCodeForbidden)
}
