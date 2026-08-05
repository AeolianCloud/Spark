// Package middleware provides gin middleware shared by all routes.
package middleware

import (
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// RequestIDKey is the gin context key holding the request id.
const RequestIDKey = "request_id"

// XRequestIDHeader is the header used to propagate the request id.
const XRequestIDHeader = "X-Request-ID"

// XMSErrorCodeHeader is the response header mirroring the error code of an
// error response body (see docs/api-errors.md). It lives in middleware, not
// in the handlers package, because middleware must stay independent of
// handlers: handlers already import middleware, so placing the constant in
// handlers would create an import cycle.
const XMSErrorCodeHeader = "x-ms-error-code"

// maxRequestIDLen caps client-supplied request ids; longer values are
// rejected so an attacker cannot inflate log lines or the propagated header.
const maxRequestIDLen = 64

// requestIDPattern is the accepted charset of client-supplied request ids.
// The charset is deliberately narrow: whitespace, control characters and
// shell/header separators are excluded so a client cannot smuggle log
// injection or header-splitting payloads into access logs and responses.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validRequestID reports whether a client-supplied request id is acceptable:
// non-empty, at most maxRequestIDLen characters and matching requestIDPattern.
// Invalid ids are discarded and replaced by a generated one (see RequestID).
func validRequestID(rid string) bool {
	return rid != "" && len(rid) <= maxRequestIDLen && requestIDPattern.MatchString(rid)
}

// RequestID generates or forwards a request id and stores it in the context.
// A client-supplied id is passed through only when validRequestID accepts it;
// otherwise a fresh uuid is generated. The accepted id is always echoed back
// in the response header, so both sides agree on the id to correlate with.
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

// errCodeInternal is the error code of the generic panic response. It
// duplicates CodeInternal from the handlers package because middleware must
// not import handlers (handlers import middleware, so sharing the constant
// would create an import cycle). The code is part of the documented API
// contract (docs/api-errors.md).
const errCodeInternal = "internal_error"

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
