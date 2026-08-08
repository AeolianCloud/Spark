package middleware

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"

	"spark/model"
)

// IdentityKey 是 gin context 中存放已认证身份（Identity）的键（设计 D4）。
const IdentityKey = "identity"

// 身份域常量（JWT role claim 取值，设计 D2）。middleware 不能 import
// service（保持该包对 handlers/service 的独立性，与 errCodeInternal 的
// 做法一致），因此与 service 包的 RoleAdmin/RoleUser 各自定义一份，
// 一致性由 api 包的锁定测试守护（见 api/router_test.go TestRoleConstantsLocked）。
const (
	RoleAdmin = "admin"
	RoleUser  = "user"

	// 鉴权错误码（见 docs/api-errors.md）：handlers 包同值常量由
	// api 包的锁定测试守护，与 errCodeInternal 模式一致。
	errCodeUnauthorized = "unauthorized"
	errCodeForbidden    = "forbidden"

	// unauthorizedMessage 是鉴权失败的统一对外消息：令牌缺失/非法/过期/
	// 账号不存在/禁用一律返回该消息，不泄露具体失败原因（设计 D4）。
	unauthorizedMessage = "unauthorized"

	// jwtSigningMethod 是校验 JWT 唯一允许的签名算法（与 service 签发
	// 算法 HS256 严格对齐）。
	jwtSigningMethod = "HS256"
)

// Identity 是已通过鉴权的身份（设计 D4）：Role 为身份域（admin/user），
// ID 为身份在对应表（admins/users）中的 ID。
type Identity struct {
	Role string
	ID   int64
}

// identityClaims 是中间件解析的 JWT 声明，与 service 签发的 claims 结构
// 一致（设计 D2：sub=身份 ID 字符串、role=身份域）。
type identityClaims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

// CredentialRepository 是 requireAuth 按角色校验身份有效性所需的最小查询
// 接口（repository.AuthRepository 满足该接口）。校验失败（账号不存在/
// 用户被禁用）时仓库返回 pgx.ErrNoRows。
type CredentialRepository interface {
	GetAdminByID(ctx context.Context, id int64) (*model.Admin, error)
	GetUserByID(ctx context.Context, id int64) (*model.User, error)
}

// RequireAuth 校验 Authorization: Bearer JWT（设计 D4）：解析并校验签名/
// 过期/角色 → 按 role 查库校验身份仍有效（user 需 status=enabled，admin
// 需账号存在；admins 表无 status 列）→ 身份注入 gin.Context（IdentityKey）。
// 令牌缺失、非法、过期、账号不存在或被禁用一律 401 unauthorized（消息
// 统一，不泄露原因）；查库本身失败（非 ErrNoRows）返回 500，不伪装成
// 鉴权失败。
func RequireAuth(secret []byte, repo CredentialRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		authz := c.GetHeader("Authorization")
		tokenString, ok := strings.CutPrefix(authz, "Bearer ")
		if !ok || tokenString == "" {
			abortUnauthorized(c)
			return
		}

		var claims identityClaims
		token, err := jwt.ParseWithClaims(tokenString, &claims,
			func(t *jwt.Token) (any, error) {
				return secret, nil
			},
			// 收紧签名算法白名单：仅 HS256（与签发算法严格对齐，L2），
			// 拒绝 alg=none 及非对称算法（防算法混淆攻击）。
			jwt.WithValidMethods([]string{jwtSigningMethod}),
			// 强制 exp 必须存在（无 exp 的令牌直接拒绝，L1）。
			jwt.WithExpirationRequired(),
			// 校验 iat 声明（早于签发时间的令牌视为无效）。
			jwt.WithIssuedAt(),
		)
		if err != nil || !token.Valid {
			abortUnauthorized(c)
			return
		}

		// sub 必须是正整数 ID（0/负数/非数字一律拒绝，L3）。
		id, err := strconv.ParseInt(claims.Subject, 10, 64)
		if err != nil || id <= 0 {
			abortUnauthorized(c)
			return
		}

		ctx := c.Request.Context()
		switch claims.Role {
		case RoleUser:
			user, err := repo.GetUserByID(ctx, id)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					abortUnauthorized(c)
					return
				}
				abortInternal(c)
				return
			}
			if user.Status != model.UserStatusEnabled {
				abortUnauthorized(c)
				return
			}
		case RoleAdmin:
			if _, err := repo.GetAdminByID(ctx, id); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					abortUnauthorized(c)
					return
				}
				abortInternal(c)
				return
			}
		default:
			abortUnauthorized(c)
			return
		}

		c.Set(IdentityKey, Identity{Role: claims.Role, ID: id})
		c.Next()
	}
}

// RequireAdmin 仅放行管理员身份（设计 D4），必须在 requireAuth 之后使用
// （依赖其注入的身份）。用户令牌访问管理员接口返回 403 forbidden；身份
// 缺失（中间件挂载顺序错误）返回 401。
func RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		ident, ok := c.Get(IdentityKey)
		if !ok {
			abortUnauthorized(c)
			return
		}
		if id, ok := ident.(Identity); !ok || id.Role != RoleAdmin {
			abortForbidden(c)
			return
		}
		c.Next()
	}
}

// abortUnauthorized 以统一错误契约终止请求（401，与 Recovery 的响应结构
// 一致并携带 x-ms-error-code 头）。
func abortUnauthorized(c *gin.Context) {
	c.Header(XMSErrorCodeHeader, errCodeUnauthorized)
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"error": gin.H{"code": errCodeUnauthorized, "message": unauthorizedMessage},
	})
}

// abortForbidden 以统一错误契约终止请求（403）。
func abortForbidden(c *gin.Context) {
	c.Header(XMSErrorCodeHeader, errCodeForbidden)
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
		"error": gin.H{"code": errCodeForbidden, "message": "forbidden"},
	})
}

// abortInternal 以统一错误契约终止请求（500）：鉴权链路中的非鉴权类
// 故障（如数据库不可用）不伪装成 401，返回通用 internal_error。
func abortInternal(c *gin.Context) {
	c.Header(XMSErrorCodeHeader, errCodeInternal)
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
		"error": gin.H{"code": errCodeInternal, "message": "internal server error"},
	})
}
