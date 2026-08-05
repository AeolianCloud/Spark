package handlers

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark/api/middleware"
	"spark/database"
)

// HealthHandler serves liveness/readiness checks.
type HealthHandler struct {
	pool *pgxpool.Pool
}

// NewHealthHandler creates a HealthHandler backed by the given pool.
func NewHealthHandler(pool *pgxpool.Pool) *HealthHandler {
	return &HealthHandler{pool: pool}
}

// Healthz reports service and database status.
// GET /healthz
// 健康探活不是业务 API：200 正常响应不携带错误契约头；503 degraded 状态
// 携带 x-ms-error-code: service_unavailable 头（见 docs/api-errors.md），
// 便于探活器与负载均衡统一判断服务不可用。
func (h *HealthHandler) Healthz(c *gin.Context) {
	if err := database.Ping(c.Request.Context(), h.pool); err != nil {
		slog.Error("healthz: database ping failed", "error", err)
		c.Header(middleware.XMSErrorCodeHeader, CodeServiceDown)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status":   "degraded",
			"database": "down",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":   "ok",
		"database": "up",
	})
}
