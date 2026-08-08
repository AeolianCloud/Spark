package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"spark/model"
)

// 身份域与 JWT 常量（设计 D2/D4）。
const (
	// RoleAdmin：管理员身份域（admins 表）。
	RoleAdmin = "admin"
	// RoleUser：前台用户身份域（users 表）。
	RoleUser = "user"

	// tokenTTL 是签发的 JWT 有效期（设计 D2：24h，无刷新机制，过期重新登录）。
	tokenTTL = 24 * time.Hour

	// invalidCredentialsMessage 是凭证无效的统一对外消息（设计 D4）：
	// 账号不存在、密码错误与账号禁用均返回同一消息，不泄露账号存在性
	// 与禁用状态。
	invalidCredentialsMessage = "invalid credentials"
	// invalidTokenMessage 是令牌无效的统一对外消息（非法/过期/篡改/角色
	// 不合法），同样不泄露具体失败原因。
	invalidTokenMessage = "invalid token"

	// dummyBcryptHash 是预生成的固定 bcrypt 哈希（bcrypt.DefaultCost，
	// 明文为 "spark-dummy-credential"），仅供失败路径的等耗时 dummy
	// 比对（security-reviewer M1：防用户枚举时序侧信道）。系统中不存在
	// 任何账号的密码与之匹配，明文本身也无实际含义。
	dummyBcryptHash = "$2a$10$Hj4OIklE6z1t5./Z3hNS4O/H2TxMFP6PRm.msNzAT4h2y6pJDbtoa"

	// jwtSigningMethod 是签/验 JWT 唯一允许的签名算法（HS256）。
	jwtSigningMethod = "HS256"
)

// Claims 是 Spark JWT 的声明负载（设计 D2）：sub 为身份域（admins/users
// 表）内的 ID 字符串，role 为身份域（admin/user），iat/exp 由
// RegisteredClaims 承载。
type Claims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

// AuthRepository 是 AuthService 依赖的凭证查询接口（登录所需的最小集，
// 设计 D4）。repository.AuthRepository 满足该接口；任务 5.x 扩展 User
// 仓储做 CRUD 时本接口保持不变。
type AuthRepository interface {
	GetAdminByUsername(ctx context.Context, username string) (*model.Admin, error)
	GetUserByUsername(ctx context.Context, username string) (*model.User, error)
}

// AuthService 实现双身份认证核心（设计 D1/D2/D4）：密码 bcrypt 哈希与
// 校验、JWT 签发（HS256）与解析，以及双身份登录。
type AuthService struct {
	// secret 是 HMAC-SHA256 签名密钥（config auth.jwt_secret），
	// 只存在于内存，绝不写入日志或响应。
	secret []byte
	repo   AuthRepository
}

// NewAuthService 创建 AuthService。secret 为空时返回错误（防御性：生产
// 路径由 config.validate 保证 jwt_secret 非空且足够长，见 G1）。
func NewAuthService(secret []byte, repo AuthRepository) (*AuthService, error) {
	if len(secret) == 0 {
		return nil, errors.New("auth: jwt secret is empty")
	}
	return &AuthService{secret: secret, repo: repo}, nil
}

// HashPassword 使用 bcrypt（默认 cost）生成密码的不可逆哈希（设计 D1：
// 登录密码只需比对，不加密存储，防拖库逆向）。
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("auth: hash password: %w", err)
	}
	return string(hash), nil
}

// VerifyPassword 校验明文密码是否与 bcrypt 哈希匹配（哈希非法时返回 false，
// 不向调用方暴露原因）。
func VerifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// LoginResult 是一次成功登录的返回负载（设计 D4）。
type LoginResult struct {
	// Role 是身份域（admin/user）。
	Role string
	// ID 是身份在对应表（admins/users）中的 ID。
	ID       int64
	Username string
	// Name 仅用户身份域有值（users.name）；管理员恒为空。
	Name  string
	Token string
}

// LoginAdmin 校验管理员凭证并签发 admin JWT（设计 D4）。账号不存在与
// 密码错误统一返回 unauthorized，不区分具体原因。
func (s *AuthService) LoginAdmin(ctx context.Context, username, password string) (*LoginResult, error) {
	admin, err := s.repo.GetAdminByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 时序均衡（M1）：账号不存在也执行一次 bcrypt 比对，使失败
			// 路径耗时与真实校验一致，防用户枚举。
			VerifyPassword(dummyBcryptHash, password)
			return nil, unauthorizedf("%s", invalidCredentialsMessage)
		}
		return nil, fmt.Errorf("auth: admin login: get admin: %w", err)
	}
	if !VerifyPassword(admin.PasswordHash, password) {
		return nil, unauthorizedf("%s", invalidCredentialsMessage)
	}
	token, err := s.signToken(RoleAdmin, admin.ID)
	if err != nil {
		return nil, fmt.Errorf("auth: admin login: sign token: %w", err)
	}
	return &LoginResult{Role: RoleAdmin, ID: admin.ID, Username: admin.Username, Token: token}, nil
}

// LoginUser 校验用户凭证与启用状态并签发 user JWT（设计 D4）：账号不存在、
// 密码错误与禁用（status != enabled）统一返回 unauthorized，消息一致，
// 不泄露具体原因。
func (s *AuthService) LoginUser(ctx context.Context, username, password string) (*LoginResult, error) {
	user, err := s.repo.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 时序均衡（M1）：账号不存在也执行一次 bcrypt 比对。
			VerifyPassword(dummyBcryptHash, password)
			return nil, unauthorizedf("%s", invalidCredentialsMessage)
		}
		return nil, fmt.Errorf("auth: user login: get user: %w", err)
	}
	if user.Status != model.UserStatusEnabled {
		// 时序均衡（M1）：禁用账号也执行一次 bcrypt 比对，与正常校验
		// 保持耗时一致。
		VerifyPassword(dummyBcryptHash, password)
		return nil, unauthorizedf("%s", invalidCredentialsMessage)
	}
	if !VerifyPassword(user.PasswordHash, password) {
		return nil, unauthorizedf("%s", invalidCredentialsMessage)
	}
	token, err := s.signToken(RoleUser, user.ID)
	if err != nil {
		return nil, fmt.Errorf("auth: user login: sign token: %w", err)
	}
	return &LoginResult{Role: RoleUser, ID: user.ID, Username: user.Username, Name: user.Name, Token: token}, nil
}

// signToken 用 HS256 签发有效期 tokenTTL 的 JWT（设计 D2）。
func (s *AuthService) signToken(role string, id int64) (string, error) {
	now := time.Now()
	claims := Claims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatInt(id, 10),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(tokenTTL)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
}

// ParseToken 校验 JWT 签名（仅接受 HS256）与声明（exp 必填、iat、role、
// sub 为正整数 ID），返回 Claims。任何失败（签名错误、过期、篡改、算法
// 替换、角色不合法、sub 非正整数）统一返回 unauthorized，不暴露具体原因。
func (s *AuthService) ParseToken(tokenString string) (*Claims, error) {
	var claims Claims
	token, err := jwt.ParseWithClaims(tokenString, &claims,
		func(t *jwt.Token) (any, error) {
			return s.secret, nil
		},
		// 收紧签名算法白名单：仅 HS256 与签发算法严格对齐，拒绝
		// alg=none 及非对称算法（防算法混淆攻击，L2）。
		jwt.WithValidMethods([]string{jwtSigningMethod}),
		// 强制 exp 必须存在（无 exp 的令牌直接拒绝，L1）。
		jwt.WithExpirationRequired(),
		// 校验 iat 声明（早于签发时间的令牌视为无效）。
		jwt.WithIssuedAt(),
	)
	if err != nil || !token.Valid {
		return nil, unauthorizedf("%s", invalidTokenMessage)
	}
	if claims.Role != RoleAdmin && claims.Role != RoleUser {
		return nil, unauthorizedf("%s", invalidTokenMessage)
	}
	// sub 必须是正整数 ID（0/负数/非数字一律拒绝，L3）。
	if id, err := strconv.ParseInt(claims.Subject, 10, 64); err != nil || id <= 0 {
		return nil, unauthorizedf("%s", invalidTokenMessage)
	}
	return &claims, nil
}
