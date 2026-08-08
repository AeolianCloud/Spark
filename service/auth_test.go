package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"

	"spark/model"
)

// testJWTSecret 是 auth 测试共用的固定密钥（≥32 字符，模拟 config 下限）。
const testJWTSecret = "test-jwt-secret-0123456789abcdefghijklmnopqrstuv"

// fakeAuthRepository 是供测试使用的可脚本化 AuthRepository。
type fakeAuthRepository struct {
	admins []model.Admin
	users  []model.User
	err    error
}

func (f *fakeAuthRepository) GetAdminByUsername(_ context.Context, username string) (*model.Admin, error) {
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

func (f *fakeAuthRepository) GetUserByUsername(_ context.Context, username string) (*model.User, error) {
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

// newTestAuthService 构建用固定密钥与 fake 仓储支撑的 AuthService。
func newTestAuthService(t *testing.T, repo AuthRepository) *AuthService {
	t.Helper()
	svc, err := NewAuthService([]byte(testJWTSecret), repo)
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}
	return svc
}

// mustHash 生成密码的 bcrypt 哈希，测试失败时直接终止。
func mustHash(t *testing.T, password string) string {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return hash
}

func TestNewAuthServiceRejectsEmptySecret(t *testing.T) {
	// 空密钥必须被拒绝（防御性：生产由 config.validate 保证非空）。
	if _, err := NewAuthService(nil, &fakeAuthRepository{}); err == nil {
		t.Fatal("NewAuthService: want error for empty secret, got nil")
	}
}

func TestHashPasswordAndVerify(t *testing.T) {
	// bcrypt 哈希：可校验、不可逆（不含明文）、随机盐（同密码两次哈希不同）。
	hash := mustHash(t, "s3cret")
	if hash == "s3cret" || !strings.HasPrefix(hash, "$2") {
		t.Fatalf("hash = %q, want bcrypt format with random salt", hash)
	}
	if !VerifyPassword(hash, "s3cret") {
		t.Error("VerifyPassword: want true for correct password")
	}
	if VerifyPassword(hash, "wrong") {
		t.Error("VerifyPassword: want false for wrong password")
	}
	// 非法哈希一律不通过，不 panic。
	if VerifyPassword("not-a-bcrypt-hash", "s3cret") {
		t.Error("VerifyPassword: want false for malformed hash")
	}
	// 同密码两次哈希不同（随机盐），但都能校验通过。
	hash2 := mustHash(t, "s3cret")
	if hash == hash2 {
		t.Error("two hashes of the same password must differ (random salt)")
	}
	if !VerifyPassword(hash2, "s3cret") {
		t.Error("VerifyPassword: want true for second hash")
	}
}

func TestSignAndParseToken(t *testing.T) {
	// 签发后能解析回读全部声明（sub/role/iat/exp 24h）。
	svc := newTestAuthService(t, &fakeAuthRepository{})
	token, err := svc.signToken(RoleUser, 42)
	if err != nil {
		t.Fatalf("signToken: %v", err)
	}
	claims, err := svc.ParseToken(token)
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	if claims.Subject != "42" || claims.Role != RoleUser {
		t.Fatalf("claims = %+v, want sub 42 role user", claims)
	}
	if !claims.ExpiresAt.Time.After(time.Now().Add(tokenTTL - 2*time.Minute)) {
		t.Errorf("expires_at = %v, want ~24h from now", claims.ExpiresAt.Time)
	}
	if claims.IssuedAt.Time.After(time.Now().Add(time.Minute)) {
		t.Errorf("issued_at = %v, want ~now", claims.IssuedAt.Time)
	}
}

func TestParseTokenRejectsExpired(t *testing.T) {
	// 过期令牌（exp 在过去）必须被拒绝。
	svc := newTestAuthService(t, &fakeAuthRepository{})
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		Role: RoleUser,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "42",
			IssuedAt:  jwt.NewNumericDate(now.Add(-2 * tokenTTL)),
			ExpiresAt: jwt.NewNumericDate(now.Add(-time.Hour)),
		},
	})
	signed, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	if _, err := svc.ParseToken(signed); err == nil {
		t.Fatal("ParseToken: want error for expired token, got nil")
	}
}

func TestParseTokenRejectsTampered(t *testing.T) {
	// 篡改（改 payload 的 sub 或 role）必须被拒绝（签名不匹配）。
	svc := newTestAuthService(t, &fakeAuthRepository{})
	token, err := svc.signToken(RoleAdmin, 7)
	if err != nil {
		t.Fatalf("signToken: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token = %q, want 3 segments", token)
	}
	// 把 sub 从 7 改成 8（payload 篡改，签名失效）。
	tampered := parts[0] + "." + "eyJzdWIiOiI4In0" + "." + parts[2]
	if _, err := svc.ParseToken(tampered); err == nil {
		t.Fatal("ParseToken: want error for tampered payload, got nil")
	}
	// 换密钥签发的令牌（同一 payload 不同签名）同样被拒。
	other, err := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		Role: RoleUser,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "42",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(tokenTTL)),
		},
	}).SignedString([]byte(strings.Repeat("x", 44)))
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	if _, err := svc.ParseToken(other); err == nil {
		t.Fatal("ParseToken: want error for token signed with different key, got nil")
	}
}

func TestParseTokenRejectsWrongRole(t *testing.T) {
	// role 取值必须严格是 admin/user：伪造角色（如 superuser）被拒绝。
	svc := newTestAuthService(t, &fakeAuthRepository{})
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		Role: "superuser",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "1",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(tokenTTL)),
		},
	})
	signed, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	if _, err := svc.ParseToken(signed); err == nil {
		t.Fatal("ParseToken: want error for unknown role, got nil")
	}
}

func TestParseTokenRejectsNonNumericSub(t *testing.T) {
	// sub 必须是合法数字 ID：非数字 sub（如用户名）被拒绝。
	svc := newTestAuthService(t, &fakeAuthRepository{})
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		Role: RoleUser,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "alice",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(tokenTTL)),
		},
	})
	signed, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	if _, err := svc.ParseToken(signed); err == nil {
		t.Fatal("ParseToken: want error for non-numeric sub, got nil")
	}
}

func TestParseTokenRejectsNonPositiveSub(t *testing.T) {
	// sub 必须是正整数 ID：0 与负数一律拒绝（L3），防止 sub=0 之类的
	// 伪造身份绕过按 ID 的鉴权查询。
	svc := newTestAuthService(t, &fakeAuthRepository{})
	for _, sub := range []string{"0", "-1"} {
		now := time.Now()
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
			Role: RoleUser,
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:   sub,
				IssuedAt:  jwt.NewNumericDate(now),
				ExpiresAt: jwt.NewNumericDate(now.Add(tokenTTL)),
			},
		})
		signed, err := token.SignedString([]byte(testJWTSecret))
		if err != nil {
			t.Fatalf("SignedString(%s): %v", sub, err)
		}
		if _, err := svc.ParseToken(signed); err == nil {
			t.Fatalf("ParseToken: want error for sub %q, got nil", sub)
		}
	}
}

func TestParseTokenRejectsMissingExp(t *testing.T) {
	// 无 exp 声明的令牌必须被拒绝（L1：强制 exp 存在，防止永不过期的
	// 伪造令牌）。
	svc := newTestAuthService(t, &fakeAuthRepository{})
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		Role: RoleUser,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:  "42",
			IssuedAt: jwt.NewNumericDate(time.Now()),
		},
	})
	signed, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	if _, err := svc.ParseToken(signed); err == nil {
		t.Fatal("ParseToken: want error for token without exp, got nil")
	}
}

func TestParseTokenRejectsNonHS256Algorithm(t *testing.T) {
	// 签名算法白名单仅 HS256：HS512 签发的令牌即使使用同一密钥也必须
	// 被拒绝（L2：防算法混淆）。
	svc := newTestAuthService(t, &fakeAuthRepository{})
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS512, Claims{
		Role: RoleUser,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "42",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(tokenTTL)),
		},
	})
	signed, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	if _, err := svc.ParseToken(signed); err == nil {
		t.Fatal("ParseToken: want error for HS512 token, got nil")
	}
}

func TestDummyBcryptHashBehavior(t *testing.T) {
	// dummy 哈希必须是合法 bcrypt 哈希、校验不 panic，且不与常见明文
	// 密码匹配（M1 时序均衡的基础：失败路径的 dummy 比对恒为失败）。
	if !strings.HasPrefix(dummyBcryptHash, "$2") {
		t.Fatalf("dummyBcryptHash = %q, want bcrypt format", dummyBcryptHash)
	}
	for _, pw := range []string{"", "wrong", "123456", "password"} {
		if VerifyPassword(dummyBcryptHash, pw) {
			t.Fatalf("VerifyPassword(dummy, %q): want false", pw)
		}
	}
}

func TestLoginUserSuccess(t *testing.T) {
	// 合法凭证：返回 user JWT 与身份信息。
	hash := mustHash(t, "pw-123")
	repo := &fakeAuthRepository{users: []model.User{
		{ID: 2, Username: "alice", PasswordHash: hash, Name: "Alice", Status: model.UserStatusEnabled},
	}}
	svc := newTestAuthService(t, repo)
	res, err := svc.LoginUser(context.Background(), "alice", "pw-123")
	if err != nil {
		t.Fatalf("LoginUser: %v", err)
	}
	if res.Role != RoleUser || res.ID != 2 || res.Username != "alice" || res.Name != "Alice" || res.Token == "" {
		t.Fatalf("res = %+v, want user 2 alice with token", res)
	}
	claims, err := svc.ParseToken(res.Token)
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	if claims.Subject != "2" || claims.Role != RoleUser {
		t.Fatalf("claims = %+v, want sub 2 role user", claims)
	}
}

func TestLoginUserUnknownAccount(t *testing.T) {
	// 账号不存在：unauthorized，消息与密码错误一致（不泄露账号存在性）。
	repo := &fakeAuthRepository{}
	svc := newTestAuthService(t, repo)
	_, err := svc.LoginUser(context.Background(), "nobody", "pw-123")
	assertUnauthorized(t, err)
}

func TestLoginUserWrongPassword(t *testing.T) {
	hash := mustHash(t, "pw-123")
	repo := &fakeAuthRepository{users: []model.User{
		{ID: 2, Username: "alice", PasswordHash: hash, Name: "Alice", Status: model.UserStatusEnabled},
	}}
	svc := newTestAuthService(t, repo)
	_, err := svc.LoginUser(context.Background(), "alice", "wrong")
	assertUnauthorized(t, err)
}

func TestLoginUserDisabled(t *testing.T) {
	// 禁用用户：即使密码正确也拒绝，且消息与凭证无效一致（任务 3.4）。
	hash := mustHash(t, "pw-123")
	repo := &fakeAuthRepository{users: []model.User{
		{ID: 2, Username: "alice", PasswordHash: hash, Name: "Alice", Status: model.UserStatusDisabled},
	}}
	svc := newTestAuthService(t, repo)
	_, err := svc.LoginUser(context.Background(), "alice", "pw-123")
	assertUnauthorized(t, err)
}

func TestLoginUserRepoError(t *testing.T) {
	// 仓储故障是内部错误（500 语义），不是 unauthorized——防止把 DB 故障
	// 伪装成凭证错误。
	repo := &fakeAuthRepository{err: errors.New("db down")}
	svc := newTestAuthService(t, repo)
	_, err := svc.LoginUser(context.Background(), "alice", "pw")
	if err == nil {
		t.Fatal("LoginUser: want error for repo failure, got nil")
	}
	var serr *Error
	if errors.As(err, &serr) {
		t.Fatalf("err = %v, want non-service error (kind %v)", err, serr.Kind)
	}
}

func TestLoginAdminSuccess(t *testing.T) {
	hash := mustHash(t, "admin-pw")
	repo := &fakeAuthRepository{admins: []model.Admin{
		{ID: 1, Username: "root", PasswordHash: hash},
	}}
	svc := newTestAuthService(t, repo)
	res, err := svc.LoginAdmin(context.Background(), "root", "admin-pw")
	if err != nil {
		t.Fatalf("LoginAdmin: %v", err)
	}
	if res.Role != RoleAdmin || res.ID != 1 || res.Username != "root" || res.Token == "" {
		t.Fatalf("res = %+v, want admin 1 root with token", res)
	}
	claims, err := svc.ParseToken(res.Token)
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	if claims.Subject != "1" || claims.Role != RoleAdmin {
		t.Fatalf("claims = %+v, want sub 1 role admin", claims)
	}
}

func TestLoginAdminUnknownAccount(t *testing.T) {
	svc := newTestAuthService(t, &fakeAuthRepository{})
	_, err := svc.LoginAdmin(context.Background(), "nobody", "pw")
	assertUnauthorized(t, err)
}

func TestLoginAdminWrongPassword(t *testing.T) {
	hash := mustHash(t, "admin-pw")
	repo := &fakeAuthRepository{admins: []model.Admin{
		{ID: 1, Username: "root", PasswordHash: hash},
	}}
	svc := newTestAuthService(t, repo)
	_, err := svc.LoginAdmin(context.Background(), "root", "wrong")
	assertUnauthorized(t, err)
}

// assertUnauthorized 断言 err 是 KindUnauthorized 且消息为统一凭证消息。
func assertUnauthorized(t *testing.T, err error) {
	t.Helper()
	var serr *Error
	if !errors.As(err, &serr) {
		t.Fatalf("err = %T, want *service.Error", err)
	}
	if serr.Kind != KindUnauthorized {
		t.Fatalf("err kind = %v, want KindUnauthorized", serr.Kind)
	}
	if serr.Message != invalidCredentialsMessage {
		t.Fatalf("err message = %q, want unified %q (不泄露具体原因)",
			serr.Message, invalidCredentialsMessage)
	}
}
