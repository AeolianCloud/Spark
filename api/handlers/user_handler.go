package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"spark/model"
	"spark/service"
)

// CodeUserHasResources 是删除用户时其名下仍关联虚拟机等资源时的错误码
// （409，设计 D6 的"有资源禁删"），区别于一般资源冲突的 conflict。
const CodeUserHasResources = "user_has_resources"

// mapUserServiceError 将 service 层错误映射为统一的 API 错误契约：
// 共享类型（bad_request/not_found/conflict）委托给 mapServiceError，
// 用户域的错误类型在这里映射。
func mapUserServiceError(err error) error {
	var serr *service.Error
	if !errors.As(err, &serr) {
		return mapServiceError(err)
	}
	switch serr.Kind {
	case service.KindUserHasResources:
		return NewError(http.StatusConflict, CodeUserHasResources, serr.Message)
	default:
		return mapServiceError(err)
	}
}

// UserHandler 提供 /users 路由（仅管理员令牌可访问，requireAdmin 由
// router 在挂载时注入，设计 D6）。
type UserHandler struct {
	svc *service.UserService
}

// NewUserHandler 创建由 svc 支撑的 UserHandler。
func NewUserHandler(svc *service.UserService) *UserHandler {
	return &UserHandler{svc: svc}
}

// RegisterUsersRoutes 在 rg 上挂载用户管理路由（管理员专用，设计 D6）：
//
//   - POST /users：创建用户（201 + Location 指向新资源）
//   - GET /users：用户列表（limit/offset 分页 + X-Total-Count）
//   - GET /users/:id：用户详情
//   - PUT /users/:id：修改（name / 重置密码）
//   - DELETE /users/:id：删除（名下有关联资源 → 409）
//   - PUT /users/:id/status：启用/禁用切换
//
// :id 恒为数字本地行 id（users.id），非法值由 parseIDParam 拒绝为 400。
func RegisterUsersRoutes(rg *gin.RouterGroup, svc *service.UserService) {
	h := NewUserHandler(svc)
	rg.POST("", Handler(h.Create))
	rg.GET("", Handler(h.List))
	rg.GET("/:id", Handler(h.Get))
	rg.PUT("/:id", Handler(h.Update))
	rg.DELETE("/:id", Handler(h.Delete))
	rg.PUT("/:id/status", Handler(h.SetStatus))
}

// userResponse 是公开的用户负载（设计 D6）：id/username/name/status/
// created_at/updated_at。password 永不包含在响应中——即使 model.User 的
// PasswordHash 已有 json:"-" 兜底，响应结构仍不声明该字段（双保险，
// 防未来实体字段演进破坏安全边界）。
type userResponse struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// toUserResponse 将 model.User 行转换为公开负载（丢弃 password_hash）。
func toUserResponse(u *model.User) userResponse {
	return userResponse{
		ID:        u.ID,
		Username:  u.Username,
		Name:      u.Name,
		Status:    u.Status,
		CreatedAt: u.CreatedAt,
		UpdatedAt: u.UpdatedAt,
	}
}

// createUserRequest 是 POST /users 的请求体：username/password 必填
// （校验位于 service 层），name 可选（缺省为空字符串）。
type createUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Name     string `json:"name"`
}

// Create 处理 POST /users：创建用户并返回 201 + 用户信息（不含密码）。
// username 重复 → 409 conflict。
func (h *UserHandler) Create(c *gin.Context) error {
	var req createUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	user, err := h.svc.CreateUser(c.Request.Context(), req.Username, req.Password, req.Name)
	if err != nil {
		return mapUserServiceError(err)
	}
	c.Header("Location", fmt.Sprintf("/users/%d", user.ID))
	c.JSON(http.StatusCreated, toUserResponse(user))
	return nil
}

// List 处理 GET /users：按共享的 limit/offset 查询参数分页返回用户
// 列表，X-Total-Count 头携带匹配用户总数。
func (h *UserHandler) List(c *gin.Context) error {
	limit, offset, err := parsePagination(c)
	if err != nil {
		return err
	}
	users, total, err := h.svc.ListUsers(c.Request.Context(), limit, offset)
	if err != nil {
		return mapUserServiceError(err)
	}
	out := make([]userResponse, 0, len(users))
	for i := range users {
		out = append(out, toUserResponse(&users[i]))
	}
	setTotalCount(c, total)
	c.JSON(http.StatusOK, out)
	return nil
}

// Get 处理 GET /users/:id：返回用户详情；用户不存在 → 404 not_found。
func (h *UserHandler) Get(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	user, err := h.svc.GetUser(c.Request.Context(), id)
	if err != nil {
		return mapUserServiceError(err)
	}
	c.JSON(http.StatusOK, toUserResponse(user))
	return nil
}

// updateUserRequest 是 PUT /users/:id 的请求体：name 与 password 均可选，
// 但至少提供一个（password 传新值即重置密码）。
type updateUserRequest struct {
	Name     *string `json:"name"`
	Password *string `json:"password"`
}

// Update 处理 PUT /users/:id：修改 name 或重置密码，返回更新后的用户。
// 空请求体（两个字段都缺省）→ 400 bad_request；用户不存在 → 404。
func (h *UserHandler) Update(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	var req updateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	user, err := h.svc.UpdateUser(c.Request.Context(), id, req.Name, req.Password)
	if err != nil {
		return mapUserServiceError(err)
	}
	c.JSON(http.StatusOK, toUserResponse(user))
	return nil
}

// Delete 处理 DELETE /users/:id：名下存在 vms.user_id 引用 → 409
// user_has_resources；无引用 → 204 无响应体；用户不存在 → 404。
func (h *UserHandler) Delete(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	if err := h.svc.DeleteUser(c.Request.Context(), id); err != nil {
		return mapUserServiceError(err)
	}
	c.Status(http.StatusNoContent)
	return nil
}

// setUserStatusRequest 是 PUT /users/:id/status 的请求体。
type setUserStatusRequest struct {
	Status string `json:"status"`
}

// SetStatus 处理 PUT /users/:id/status：切换用户启用/禁用状态并返回
// 更新结果。非法状态取值 → 400 bad_request；用户不存在 → 404。
func (h *UserHandler) SetStatus(c *gin.Context) error {
	id, err := parseIDParam(c, "id")
	if err != nil {
		return err
	}
	var req setUserStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrBadRequest("invalid request body")
	}
	user, err := h.svc.SetUserStatus(c.Request.Context(), id, req.Status)
	if err != nil {
		return mapUserServiceError(err)
	}
	c.JSON(http.StatusOK, toUserResponse(user))
	return nil
}
