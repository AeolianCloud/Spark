// Package handlers contains HTTP handlers and the unified error contract.
package handlers

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"spark/api/middleware"
)

// Error codes used across the API.
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

// APIError is the unified error payload: {"error": {"code", "message"}}.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"-"`
}

func (e *APIError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// NewError builds an APIError with the given HTTP status and code.
// Invalid statuses (<100 or >599) fall back to 500 so APIError always
// carries a legal HTTP status code.
func NewError(status int, code, message string) *APIError {
	if status < 100 || status > 599 {
		status = http.StatusInternalServerError
	}
	return &APIError{Code: code, Message: message, Status: status}
}

// NewErrorf builds an APIError with a formatted message.
func NewErrorf(status int, code, format string, args ...any) *APIError {
	return NewError(status, code, fmt.Sprintf(format, args...))
}

// ErrBadRequest returns a 400 APIError.
func ErrBadRequest(message string) *APIError {
	return NewError(http.StatusBadRequest, CodeBadRequest, message)
}

// ErrNotFound returns a 404 APIError.
func ErrNotFound(message string) *APIError {
	return NewError(http.StatusNotFound, CodeNotFound, message)
}

// ErrConflict returns a 409 APIError.
func ErrConflict(message string) *APIError {
	return NewError(http.StatusConflict, CodeConflict, message)
}

// ErrUnprocessable returns a 422 APIError.
func ErrUnprocessable(message string) *APIError {
	return NewError(http.StatusUnprocessableEntity, CodeUnprocessable, message)
}

// ErrInternal returns a 500 APIError.
func ErrInternal(message string) *APIError {
	return NewError(http.StatusInternalServerError, CodeInternal, message)
}

// ErrServiceDown returns a 503 APIError.
func ErrServiceDown(message string) *APIError {
	return NewError(http.StatusServiceUnavailable, CodeServiceDown, message)
}

// Handler adapts a handler returning an error into a gin.HandlerFunc.
// Any error that is an *APIError is rendered as-is; other errors are logged
// and rendered as a generic 500 so internal details never leak. Every error
// response carries the x-ms-error-code header mirroring the body code (the
// constant lives in middleware to keep the dependency direction acyclic).
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
