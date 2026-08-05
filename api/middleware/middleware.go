// Package middleware provides gin middleware shared by all routes.
package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// RequestIDKey is the gin context key holding the request id.
const RequestIDKey = "request_id"

// XRequestIDHeader is the header used to propagate the request id.
const XRequestIDHeader = "X-Request-ID"

// RequestID generates or forwards a request id and stores it in the context.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader(XRequestIDHeader)
		if rid == "" {
			rid = uuid.NewString()
		}
		c.Set(RequestIDKey, rid)
		c.Header(XRequestIDHeader, rid)
		c.Next()
	}
}

// Logger logs one line per request with method, path, status and latency.
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
		// Avoid filling normal logs with healthcheck noise.
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

// Recovery converts panics into a unified 500 error response.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic recovered",
					"request_id", c.GetString(RequestIDKey),
					"path", c.Request.URL.Path,
					"panic", r,
				)
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{
						"code":    "internal_error",
						"message": "internal server error",
					},
				})
			}
		}()
		c.Next()
	}
}
