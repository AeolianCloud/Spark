package handlers

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

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
func (h *HealthHandler) Healthz(c *gin.Context) {
	if err := database.Ping(c.Request.Context(), h.pool); err != nil {
		slog.Error("healthz: database ping failed", "error", err)
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
