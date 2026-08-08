package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"spark/service"
)

// AuthHandler 提供双身份登录路由（设计 D4）：用户登录与管理员登录各返回
// 对应身份域的 JWT（Bearer）。
type AuthHandler struct {
	svc *service.AuthService
}

// NewAuthHandler 创建由 svc 支撑的 AuthHandler。
func NewAuthHandler(svc *service.AuthService) *AuthHandler {
	return &AuthHandler{svc: svc}
}

// RegisterAuthRoutes 挂载登录路由（公开组，不加鉴权，设计 D4）：
// POST /auth/login（用户）、POST /auth/admin/login（管理员）。
// 由 router 以 /auth 分组调用。
func RegisterAuthRoutes(rg *gin.RouterGroup, svc *service.AuthService) {
	h := NewAuthHandler(svc)
	rg.POST("/login", Handler(h.LoginUser))
	rg.POST("/admin/login", Handler(h.LoginAdmin))
}

// loginRequest 是两个登录接口共用的请求体。
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// userLoginResponse 是用户登录成功响应（设计 D4）：user JWT 与身份信息。
type userLoginResponse struct {
	Token    string `json:"token"`
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

// adminLoginResponse 是管理员登录成功响应：admin JWT 与身份信息。
type adminLoginResponse struct {
	Token    string `json:"token"`
	AdminID  int64  `json:"admin_id"`
	Username string `json:"username"`
}

// LoginUser 处理 POST /auth/login：校验用户凭证与启用状态，成功返回
// user JWT + user_id。凭证无效（含禁用）由 service 层统一映射为 401
// unauthorized（消息一致，不泄露原因）。
func (h *AuthHandler) LoginUser(c *gin.Context) error {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	res, err := h.svc.LoginUser(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		return mapServiceErrorExtended(err)
	}
	c.JSON(http.StatusOK, userLoginResponse{
		Token: res.Token, UserID: res.ID, Username: res.Username, Name: res.Name,
	})
	return nil
}

// LoginAdmin 处理 POST /auth/admin/login：校验管理员凭证，成功返回
// admin JWT + admin_id。
func (h *AuthHandler) LoginAdmin(c *gin.Context) error {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	res, err := h.svc.LoginAdmin(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		return mapServiceErrorExtended(err)
	}
	c.JSON(http.StatusOK, adminLoginResponse{
		Token: res.Token, AdminID: res.ID, Username: res.Username,
	})
	return nil
}
