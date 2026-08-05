package handlers

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// newPaginationTestCtx builds a gin context carrying the given query string.
func newPaginationTestCtx(t *testing.T, query string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest("GET", "/list?"+query, nil)
	c.Request = req
	return c
}

// TestParsePaginationDefaults covers the bare endpoint call: no query
// parameters at all.
func TestParsePaginationDefaults(t *testing.T) {
	limit, offset, err := parsePagination(newPaginationTestCtx(t, ""))
	if err != nil {
		t.Fatalf("parsePagination: %v", err)
	}
	if limit != 25 || offset != 0 {
		t.Fatalf("limit = %d offset = %d, want 25/0", limit, offset)
	}
}

// TestParsePaginationCap verifies the DoS cap: an oversized limit is
// truncated to maxPageLimit instead of being rejected.
func TestParsePaginationCap(t *testing.T) {
	limit, offset, err := parsePagination(newPaginationTestCtx(t, "limit=5000&offset=40"))
	if err != nil {
		t.Fatalf("parsePagination: %v", err)
	}
	if limit != 100 || offset != 40 {
		t.Fatalf("limit = %d offset = %d, want 100/40", limit, offset)
	}
}

// TestParsePaginationRejectsInvalid pins down the 400 cases: negative or
// non-numeric limit/offset values.
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
	// A limit of 0 is a legal (empty) page, not an error.
	if _, _, err := parsePagination(newPaginationTestCtx(t, "limit=0")); err != nil {
		t.Fatalf("limit=0: %v, want nil", err)
	}
}
