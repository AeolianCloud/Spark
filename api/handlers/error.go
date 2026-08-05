// Package handlers 包含 HTTP 处理器与统一的错误契约。
package handlers

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"spark/api/middleware"
)

// 整个 API 使用的错误码。
const (
	CodeBadRequest       = "bad_request"
	CodeNotFound         = "not_found"
	CodeMethodNotAllowed = "method_not_allowed"
	CodeConflict         = "conflict"
	CodeUnprocessable    = "unprocessable_entity"
	CodeInternal         = "internal_error"
	CodeServiceDown      = "service_unavailable"
	CodeDependencyFailed = "dependency_failed"
)

// APIError 是统一的错误负载：{"error": {"code", "message"}}。
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"-"`
}

func (e *APIError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// NewError 用给定的 HTTP 状态码和错误码构建 APIError。
// 非法的状态码（<100 或 >599）回退为 500，使 APIError 始终携带合法的 HTTP 状态码。
func NewError(status int, code, message string) *APIError {
	if status < 100 || status > 599 {
		status = http.StatusInternalServerError
	}
	return &APIError{Code: code, Message: message, Status: status}
}

// NewErrorf 构建带格式化消息的 APIError。
func NewErrorf(status int, code, format string, args ...any) *APIError {
	return NewError(status, code, fmt.Sprintf(format, args...))
}

// ErrBadRequest 返回一个 400 APIError。
func ErrBadRequest(message string) *APIError {
	return NewError(http.StatusBadRequest, CodeBadRequest, message)
}

// ErrNotFound 返回一个 404 APIError。
func ErrNotFound(message string) *APIError {
	return NewError(http.StatusNotFound, CodeNotFound, message)
}

// ErrConflict 返回一个 409 APIError。
func ErrConflict(message string) *APIError {
	return NewError(http.StatusConflict, CodeConflict, message)
}

// ErrUnprocessable 返回一个 422 APIError。
func ErrUnprocessable(message string) *APIError {
	return NewError(http.StatusUnprocessableEntity, CodeUnprocessable, message)
}

// ErrInternal 返回一个 500 APIError。
func ErrInternal(message string) *APIError {
	return NewError(http.StatusInternalServerError, CodeInternal, message)
}

// ErrServiceDown 返回一个 503 APIError。
func ErrServiceDown(message string) *APIError {
	return NewError(http.StatusServiceUnavailable, CodeServiceDown, message)
}

// Handler 将返回错误的处理器适配为 gin.HandlerFunc。
// 任何 *APIError 类型的错误按原样渲染；其他错误会被记录并渲染为通用 500，
// 确保内部细节永不泄露。每个错误响应都携带与 body 错误码一致的
// x-ms-error-code 头（该常量位于 middleware，以保持依赖方向无环）。
func Handler(fn func(c *gin.Context) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := fn(c); err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) {
				c.Header(middleware.XMSErrorCodeHeader, apiErr.Code)
				c.AbortWithStatusJSON(apiErr.Status, gin.H{"error": apiErr})
				return
			}
			slog.Error("unhandled handler error",
				"request_id", c.GetString(middleware.RequestIDKey),
				"method", c.Request.Method,
				"path", c.Request.URL.Path,
				"error", err,
			)
			c.Header(middleware.XMSErrorCodeHeader, CodeInternal)
			c.AbortWithStatusJSON(http.StatusInternalServerError,
				gin.H{"error": ErrInternal("internal server error")})
		}
	}
}
