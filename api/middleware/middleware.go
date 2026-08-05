// Package middleware 提供所有路由共享的 gin 中间件。
package middleware

import (
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// RequestIDKey 是存放 request id 的 gin context 键。
const RequestIDKey = "request_id"

// XRequestIDHeader 是用于传播 request id 的头。
const XRequestIDHeader = "X-Request-ID"

// XMSErrorCodeHeader 是镜像错误响应体错误码的响应头
// （见 docs/api-errors.md）。它位于 middleware 而非 handlers 包，
// 因为 middleware 必须保持独立于 handlers：handlers 已经 import
// middleware，把常量放在 handlers 会造成 import 环。
const XMSErrorCodeHeader = "x-ms-error-code"

// maxRequestIDLen 限制客户端提供的 request id 长度；更长的值被拒绝，
// 防止攻击者膨胀日志行或传播的头。
const maxRequestIDLen = 64

// requestIDPattern 是客户端提供的 request id 的合法字符集。
// 该字符集刻意收窄：排除空白、控制字符及 shell/header 分隔符，
// 防止客户端把日志注入或 header 拆分负载混入访问日志和响应中。
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validRequestID 报告客户端提供的 request id 是否可接受：
// 非空、不超过 maxRequestIDLen 字符且匹配 requestIDPattern。
// 非法的 id 被丢弃并替换为生成的 id（见 RequestID）。
func validRequestID(rid string) bool {
	return rid != "" && len(rid) <= maxRequestIDLen && requestIDPattern.MatchString(rid)
}

// RequestID 生成或转发 request id 并存入 context。
// 仅当 validRequestID 接受时客户端提供的 id 才会被透传；
// 否则生成全新的 uuid。被接受的 id 总是回显在响应头中，
// 使双方对所关联的 id 达成一致。
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader(XRequestIDHeader)
		if !validRequestID(rid) {
			rid = uuid.NewString()
		}
		c.Set(RequestIDKey, rid)
		c.Header(XRequestIDHeader, rid)
		c.Next()
	}
}

// Logger 每个请求记录一行日志，包含 method、path、status 与 latency。
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		status := c.Writer.Status()
		attrs := []any{
			"request_id", c.GetString(RequestIDKey),
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", status,
			"latency", time.Since(start),
			"client_ip", c.ClientIP(),
		}
		// 避免健康检查噪声填满常规日志。
		switch {
		case status >= 500:
			slog.Error("request", attrs...)
		case status >= 400:
			slog.Warn("request", attrs...)
		case c.Request.URL.Path == "/healthz":
			slog.Debug("request", attrs...)
		default:
			slog.Info("request", attrs...)
		}
	}
}

// errCodeInternal 是通用 panic 响应的错误码。它复制自 handlers 包的
// CodeInternal，因为 middleware 不能 import handlers（handlers import
// middleware，共享该常量会造成 import 环）。该错误码是文档化 API
// 契约的一部分（docs/api-errors.md）。
const errCodeInternal = "internal_error"

// Recovery 将 panic 转换为统一的 500 错误响应。
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic recovered",
					"request_id", c.GetString(RequestIDKey),
					"path", c.Request.URL.Path,
					"panic", r,
				)
				c.Header(XMSErrorCodeHeader, errCodeInternal)
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{
						"code":    errCodeInternal,
						"message": "internal server error",
					},
				})
			}
		}()
		c.Next()
	}
}
