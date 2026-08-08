package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"

	"spark/api/middleware"
	"spark/model"
	"spark/repository"
	"spark/service"
)

// userCredentialRepo 是供测试使用的可脚本化 middleware.CredentialRepository
// （requireAuth 按角色查库校验身份）。
type userCredentialRepo struct {
	admins []model.Admin
	users  []model.User
	err    error
}

func (f *userCredentialRepo) GetAdminByID(_ context.Context, id int64) (*model.Admin, error) {
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

func (f *userCredentialRepo) GetUserByID(_ context.Context, id int64) (*model.User, error) {
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

// fakeUserServiceRepo 是供测试使用的可脚本化 service.UserRepository。
type fakeUserServiceRepo struct {
	users    []model.User
	nextID   int64
	err      error
	conflict bool
	inUse    bool
}

func (f *fakeUserServiceRepo) Create(_ context.Context, username, passwordHash, name string) (*model.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.conflict {
		return nil, repository.ErrConflict
	}
	u := model.User{
		ID: f.nextID, Username: username, PasswordHash: passwordHash,
		Name: name, Status: model.UserStatusEnabled,
		CreatedAt: userTestTime, UpdatedAt: userTestTime,
	}
	f.nextID++
	f.users = append(f.users, u)
	return &u, nil
}

func (f *fakeUserServiceRepo) List(_ context.Context, limit, offset int) ([]model.User, error) {
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

func (f *fakeUserServiceRepo) Count(_ context.Context) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	return len(f.users), nil
}

func (f *fakeUserServiceRepo) GetByID(_ context.Context, id int64) (*model.User, error) {
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

func (f *fakeUserServiceRepo) Update(_ context.Context, id int64, name, passwordHash *string) (*model.User, error) {
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

func (f *fakeUserServiceRepo) Delete(_ context.Context, id int64) error {
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

func (f *fakeUserServiceRepo) SetStatus(_ context.Context, id int64, status string) (*model.User, error) {
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

// userTestTime 是 fake 用户行的固定时间戳。
var userTestTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// seedHandlerUser 在 fake 仓储中预置一个用户，返回其 id。
func seedHandlerUser(f *fakeUserServiceRepo, username, name, status string) int64 {
	u := model.User{
		ID: f.nextID, Username: username, PasswordHash: "$2a$10$hash",
		Name: name, Status: status,
		CreatedAt: userTestTime, UpdatedAt: userTestTime,
	}
	f.nextID++
	f.users = append(f.users, u)
	return u.ID
}

// signUserTestToken 用固定密钥签发测试令牌（与 middleware 解析的 claims
// 结构一致：role + sub）。
func signUserTestToken(t *testing.T, role string, id int64) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, struct {
		Role string `json:"role"`
		jwt.RegisteredClaims
	}{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatInt(id, 10),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
		},
	})
	signed, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	return signed
}

// adminToken / userToken 构造固定的有效令牌（鉴权查库由 credential repo
// 决定）。
func adminToken(t *testing.T) string { return signUserTestToken(t, middleware.RoleAdmin, 1) }
func userToken(t *testing.T) string  { return signUserTestToken(t, middleware.RoleUser, 2) }

// newUserTestRouter 构建挂载 requireAuth + requireAdmin + 用户 CRUD 路由
// 的测试引擎：用户端点必须管理员令牌才能访问（设计 D6）。
func newUserTestRouter(t *testing.T, cred *userCredentialRepo, repo *fakeUserServiceRepo) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	svc := service.NewUserService(repo)
	rg := r.Group("/users",
		middleware.RequireAuth([]byte(testJWTSecret), cred), middleware.RequireAdmin())
	RegisterUsersRoutes(rg, svc)
	return r
}

// doUsersRequest 发起带/不带 Bearer 令牌的 JSON 请求。
func doUsersRequest(t *testing.T, r *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	r.ServeHTTP(w, req)
	return w
}

// assertUsersAuthError 断言鉴权错误响应（401/403）的统一契约。
func assertUsersAuthError(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if w.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, wantStatus, w.Body.String())
	}
	if got := w.Header().Get(middleware.XMSErrorCodeHeader); got != wantCode {
		t.Errorf("x-ms-error-code = %q, want %q", got, wantCode)
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != wantCode {
		t.Errorf("body error code = %q, want %q", body.Error.Code, wantCode)
	}
}

// assertNoPasswordLeak 断言响应体不包含 password_hash / password 字段
// （密码永不回显，红线）。
func assertNoPasswordLeak(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if bytes.Contains(w.Body.Bytes(), []byte("password_hash")) || bytes.Contains(w.Body.Bytes(), []byte("password")) {
		t.Fatalf("response leaks password fields: %s", w.Body.String())
	}
}

func TestUsersRequireAdminToken(t *testing.T) {
	// 路由分层（设计 D6）：缺失令牌 401、用户令牌 403、管理员令牌放行。
	cred := &userCredentialRepo{
		admins: []model.Admin{{ID: 1}},
		users:  []model.User{{ID: 2, Status: model.UserStatusEnabled}},
	}
	r := newUserTestRouter(t, cred, &fakeUserServiceRepo{})

	t.Run("missing token 401", func(t *testing.T) {
		w := doUsersRequest(t, r, http.MethodGet, "/users", "", "")
		assertUsersAuthError(t, w, http.StatusUnauthorized, CodeUnauthorized)
	})
	t.Run("user token 403", func(t *testing.T) {
		w := doUsersRequest(t, r, http.MethodGet, "/users", userToken(t), "")
		assertUsersAuthError(t, w, http.StatusForbidden, CodeForbidden)
	})
	t.Run("admin token allowed", func(t *testing.T) {
		w := doUsersRequest(t, r, http.MethodGet, "/users", adminToken(t), "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
	})
}

func TestUsersCreate(t *testing.T) {
	// 创建用户：201 + Location + 完整用户负载（不含密码）。
	cred := &userCredentialRepo{admins: []model.Admin{{ID: 1}}}
	r := newUserTestRouter(t, cred, &fakeUserServiceRepo{nextID: 5})

	w := doUsersRequest(t, r, http.MethodPost, "/users", adminToken(t),
		`{"username":"alice","password":"pw-123","name":"Alice"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Location"); got != "/users/5" {
		t.Errorf("Location = %q, want /users/5", got)
	}
	assertNoPasswordLeak(t, w)
	var body userResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.ID != 5 || body.Username != "alice" || body.Name != "Alice" || body.Status != "enabled" {
		t.Fatalf("body = %+v, want user 5 alice/Alice/enabled", body)
	}
}

func TestUsersCreateDuplicate(t *testing.T) {
	// username 重复 → 409 conflict。
	cred := &userCredentialRepo{admins: []model.Admin{{ID: 1}}}
	r := newUserTestRouter(t, cred, &fakeUserServiceRepo{conflict: true})

	w := doUsersRequest(t, r, http.MethodPost, "/users", adminToken(t),
		`{"username":"alice","password":"pw-123"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(middleware.XMSErrorCodeHeader); got != CodeConflict {
		t.Errorf("x-ms-error-code = %q, want %q", got, CodeConflict)
	}
}

func TestUsersCreateBadBody(t *testing.T) {
	// 非法 JSON → 400 bad_request。
	cred := &userCredentialRepo{admins: []model.Admin{{ID: 1}}}
	r := newUserTestRouter(t, cred, &fakeUserServiceRepo{})

	for _, body := range []string{"", "{", `{"username":123,"password":456}`} {
		w := doUsersRequest(t, r, http.MethodPost, "/users", adminToken(t), body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, w.Code)
		}
		if got := w.Header().Get(middleware.XMSErrorCodeHeader); got != CodeBadRequest {
			t.Errorf("body %q: x-ms-error-code = %q, want %q", body, got, CodeBadRequest)
		}
	}
}

func TestUsersList(t *testing.T) {
	// 分页列表：X-Total-Count 携带总数，响应不含密码。
	cred := &userCredentialRepo{admins: []model.Admin{{ID: 1}}}
	repo := &fakeUserServiceRepo{nextID: 1}
	seedHandlerUser(repo, "alice", "Alice", model.UserStatusEnabled)
	seedHandlerUser(repo, "bob", "Bob", model.UserStatusDisabled)
	r := newUserTestRouter(t, cred, repo)

	w := doUsersRequest(t, r, http.MethodGet, "/users?limit=1&offset=1", adminToken(t), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(XTotalCountHeader); got != "2" {
		t.Errorf("X-Total-Count = %q, want 2", got)
	}
	assertNoPasswordLeak(t, w)
	var body []userResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body) != 1 || body[0].Username != "bob" || body[0].Status != "disabled" {
		t.Fatalf("body = %+v, want page with bob", body)
	}
}

func TestUsersListBadPagination(t *testing.T) {
	// 非法分页参数 → 400 bad_request（共享 parsePagination 行为）。
	cred := &userCredentialRepo{admins: []model.Admin{{ID: 1}}}
	r := newUserTestRouter(t, cred, &fakeUserServiceRepo{})

	w := doUsersRequest(t, r, http.MethodGet, "/users?limit=-1", adminToken(t), "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
}

func TestUsersGet(t *testing.T) {
	cred := &userCredentialRepo{admins: []model.Admin{{ID: 1}}}
	repo := &fakeUserServiceRepo{nextID: 1}
	id := seedHandlerUser(repo, "alice", "Alice", model.UserStatusEnabled)
	r := newUserTestRouter(t, cred, repo)

	t.Run("found 200", func(t *testing.T) {
		w := doUsersRequest(t, r, http.MethodGet, fmt.Sprintf("/users/%d", id), adminToken(t), "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		assertNoPasswordLeak(t, w)
		var body userResponse
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.ID != id || body.Username != "alice" {
			t.Fatalf("body = %+v, want alice %d", body, id)
		}
	})
	t.Run("not found 404", func(t *testing.T) {
		w := doUsersRequest(t, r, http.MethodGet, "/users/99", adminToken(t), "")
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", w.Code, w.Body.String())
		}
	})
	t.Run("bad id 400", func(t *testing.T) {
		w := doUsersRequest(t, r, http.MethodGet, "/users/abc", adminToken(t), "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
		}
	})
}

func TestUsersUpdate(t *testing.T) {
	cred := &userCredentialRepo{admins: []model.Admin{{ID: 1}}}
	repo := &fakeUserServiceRepo{nextID: 1}
	id := seedHandlerUser(repo, "alice", "Alice", model.UserStatusEnabled)
	r := newUserTestRouter(t, cred, repo)

	t.Run("update name 200", func(t *testing.T) {
		w := doUsersRequest(t, r, http.MethodPut, fmt.Sprintf("/users/%d", id), adminToken(t),
			`{"name":"Alice2"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		assertNoPasswordLeak(t, w)
		var body userResponse
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Name != "Alice2" {
			t.Fatalf("body = %+v, want name Alice2", body)
		}
	})
	t.Run("reset password 200", func(t *testing.T) {
		w := doUsersRequest(t, r, http.MethodPut, fmt.Sprintf("/users/%d", id), adminToken(t),
			`{"password":"new-pw"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		assertNoPasswordLeak(t, w)
	})
	t.Run("empty body 400", func(t *testing.T) {
		w := doUsersRequest(t, r, http.MethodPut, fmt.Sprintf("/users/%d", id), adminToken(t), `{}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
		}
	})
	t.Run("not found 404", func(t *testing.T) {
		w := doUsersRequest(t, r, http.MethodPut, "/users/99", adminToken(t), `{"name":"X"}`)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", w.Code, w.Body.String())
		}
	})
}

func TestUsersDelete(t *testing.T) {
	cred := &userCredentialRepo{admins: []model.Admin{{ID: 1}}}
	repo := &fakeUserServiceRepo{nextID: 1}
	id := seedHandlerUser(repo, "alice", "Alice", model.UserStatusEnabled)
	r := newUserTestRouter(t, cred, repo)

	t.Run("deleted 204", func(t *testing.T) {
		w := doUsersRequest(t, r, http.MethodDelete, fmt.Sprintf("/users/%d", id), adminToken(t), "")
		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body = %s", w.Code, w.Body.String())
		}
	})
	t.Run("not found 404", func(t *testing.T) {
		w := doUsersRequest(t, r, http.MethodDelete, "/users/99", adminToken(t), "")
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", w.Code, w.Body.String())
		}
	})
}

func TestUsersDeleteInUse(t *testing.T) {
	// 名下仍有虚拟机引用 → 409 user_has_resources（区别于一般 conflict）。
	cred := &userCredentialRepo{admins: []model.Admin{{ID: 1}}}
	r := newUserTestRouter(t, cred, &fakeUserServiceRepo{inUse: true})

	w := doUsersRequest(t, r, http.MethodDelete, "/users/1", adminToken(t), "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get(middleware.XMSErrorCodeHeader); got != CodeUserHasResources {
		t.Errorf("x-ms-error-code = %q, want %q", got, CodeUserHasResources)
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != CodeUserHasResources {
		t.Errorf("body error code = %q, want %q", body.Error.Code, CodeUserHasResources)
	}
}

func TestUsersSetStatus(t *testing.T) {
	cred := &userCredentialRepo{admins: []model.Admin{{ID: 1}}}
	repo := &fakeUserServiceRepo{nextID: 1}
	id := seedHandlerUser(repo, "alice", "Alice", model.UserStatusEnabled)
	r := newUserTestRouter(t, cred, repo)

	t.Run("disable 200", func(t *testing.T) {
		w := doUsersRequest(t, r, http.MethodPut, fmt.Sprintf("/users/%d/status", id), adminToken(t),
			`{"status":"disabled"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		assertNoPasswordLeak(t, w)
		var body userResponse
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Status != "disabled" {
			t.Fatalf("body = %+v, want status disabled", body)
		}
	})
	t.Run("invalid status 400", func(t *testing.T) {
		w := doUsersRequest(t, r, http.MethodPut, fmt.Sprintf("/users/%d/status", id), adminToken(t),
			`{"status":"suspended"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
		}
	})
	t.Run("not found 404", func(t *testing.T) {
		w := doUsersRequest(t, r, http.MethodPut, "/users/99/status", adminToken(t),
			`{"status":"disabled"}`)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", w.Code, w.Body.String())
		}
	})
}
