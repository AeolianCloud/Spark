package handlers

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// newPaginationTestCtx 构建一个携带给定查询字符串的 gin context。
func newPaginationTestCtx(t *testing.T, query string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest("GET", "/list?"+query, nil)
	c.Request = req
	return c
}

// TestParsePaginationDefaults 覆盖裸端点调用：没有任何查询参数。
func TestParsePaginationDefaults(t *testing.T) {
	limit, offset, err := parsePagination(newPaginationTestCtx(t, ""))
	if err != nil {
		t.Fatalf("parsePagination: %v", err)
	}
	if limit != 25 || offset != 0 {
		t.Fatalf("limit = %d offset = %d, want 25/0", limit, offset)
	}
}

// TestParsePaginationCap 验证 DoS 上限：过大的 limit 被截断为
// maxPageLimit 而不是被拒绝。
func TestParsePaginationCap(t *testing.T) {
	limit, offset, err := parsePagination(newPaginationTestCtx(t, "limit=5000&offset=40"))
	if err != nil {
		t.Fatalf("parsePagination: %v", err)
	}
	if limit != 100 || offset != 40 {
		t.Fatalf("limit = %d offset = %d, want 100/40", limit, offset)
	}
}

// TestParsePaginationRejectsInvalid 固定 400 的情形：负数或
// 非数值的 limit/offset。
func TestParsePaginationRejectsInvalid(t *testing.T) {
	for _, query := range []string{
		"limit=-1",
		"limit=abc",
		"limit=1.5",
		"offset=-3",
		"offset=x",
	} {
		if _, _, err := parsePagination(newPaginationTestCtx(t, query)); err == nil {
			t.Errorf("query %q: want an error", query)
		}
	}
	// limit 为 0 是合法的（空）分页，而非错误。
	if _, _, err := parsePagination(newPaginationTestCtx(t, "limit=0")); err != nil {
		t.Fatalf("limit=0: %v, want nil", err)
	}
}
