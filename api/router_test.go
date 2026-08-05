package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark/api/handlers"
	"spark/api/middleware"
	"spark/config"
	"spark/crypto"
)

// testRouterPool builds a lazy pool that never opens a connection, good
// enough for routing-level tests that do not touch the database.
func testRouterPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://user:pass@127.0.0.1:1/db")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// testRouterCipher builds a cipher from a deterministic 32-byte key.
func testRouterCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	cfg := config.Default()
	cfg.Crypto.EncryptionKey = base64.StdEncoding.EncodeToString(key)
	c, err := crypto.NewCipher(cfg)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

// TestRouterRegistersAllRoutes builds the full router with a lazy pool (no
// connection is ever opened) and asserts the complete route table. gin
// panics on conflicting route registrations, so merely building the router
// is the conflict check; the assertions pin down the expected paths,
// including the batch-7 /vms group.
func TestRouterRegistersAllRoutes(t *testing.T) {
	pool := testRouterPool(t)

	r := NewRouter(pool, testRouterCipher(t))

	want := []string{
		"GET /healthz",
		"POST /zones",
		"GET /zones",
		"POST /zones/:zone_id/nodes",
		"GET /zones/:zone_id/nodes",
		"PUT /nodes/:id",
		"POST /ip-pools",
		"GET /ip-pools",
		"PUT /ip-pools/:id/nodes",
		"GET /ip-pools/:id/nodes",
		"POST /storage-types",
		"GET /storage-types",
		"GET /storage-types/:id",
		"PUT /storage-types/:id",
		"DELETE /storage-types/:id",
		"POST /images",
		"GET /images",
		"POST /vms",
		"GET /vms",
		"GET /vms/:id",
		"POST /vms/:id/start",
		"POST /vms/:id/stop",
		"POST /vms/:id/restart",
		"PATCH /vms/:id",
		"DELETE /vms/:id",
	}
	got := make(map[string]bool)
	for _, route := range r.Routes() {
		got[route.Method+" "+route.Path] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("route %q is not registered", w)
		}
	}
}

// errorBody mirrors the unified error payload {"error":{"code","message"}}.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// TestRouterUnmatchedPathAndMethod verifies the unified error contract
// covers the whole API surface: unknown paths yield the 404 shape and
// known paths with an unregistered method yield 405 plus the Allow header.
func TestRouterUnmatchedPathAndMethod(t *testing.T) {
	r := NewRouter(testRouterPool(t), testRouterCipher(t))

	t.Run("unknown path returns unified 404", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/definitely/not/a/route", nil)
		r.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
		if got := w.Header().Get(middleware.XMSErrorCodeHeader); got != handlers.CodeNotFound {
			t.Errorf("x-ms-error-code header = %q, want %q", got, handlers.CodeNotFound)
		}
		var body errorBody
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Error.Code != handlers.CodeNotFound {
			t.Errorf("body error code = %q, want %q", body.Error.Code, handlers.CodeNotFound)
		}
		if body.Error.Code != w.Header().Get(middleware.XMSErrorCodeHeader) {
			t.Errorf("body error code = %q, want header value %q",
				body.Error.Code, w.Header().Get(middleware.XMSErrorCodeHeader))
		}
		if body.Error.Message == "" {
			t.Error("body error message is empty")
		}
	})

	t.Run("unregistered method returns 405 with Allow header", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPatch, "/zones", nil)
		r.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
		}
		if got := w.Header().Get(middleware.XMSErrorCodeHeader); got != handlers.CodeMethodNotAllowed {
			t.Errorf("x-ms-error-code header = %q, want %q", got, handlers.CodeMethodNotAllowed)
		}
		allow := w.Header().Get("Allow")
		if allow == "" || !strings.Contains(allow, "GET") || !strings.Contains(allow, "POST") {
			t.Errorf("Allow header = %q, want it to list GET and POST", allow)
		}
		var body errorBody
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Error.Code != handlers.CodeMethodNotAllowed {
			t.Errorf("body error code = %q, want %q", body.Error.Code, handlers.CodeMethodNotAllowed)
		}
		if body.Error.Code != w.Header().Get(middleware.XMSErrorCodeHeader) {
			t.Errorf("body error code = %q, want header value %q",
				body.Error.Code, w.Header().Get(middleware.XMSErrorCodeHeader))
		}
		if body.Error.Message == "" {
			t.Error("body error message is empty")
		}
	})

	t.Run("param path 405 Allow lists registered methods", func(t *testing.T) {
		// /zones/:zone_id/nodes 注册了 GET/POST，参数段也要能匹配。
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, "/zones/z1/nodes", nil)
		r.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
		}
		allow := w.Header().Get("Allow")
		if allow == "" || !strings.Contains(allow, "GET") || !strings.Contains(allow, "POST") {
			t.Errorf("Allow header = %q, want it to list GET and POST", allow)
		}
	})
}

// TestErrorCodeConstantsLocked guards the duplicated internal-error code
// across packages: handlers.CodeInternal and middleware.errCodeInternal
// (asserted in middleware_test.go) must both equal the documented value.
// middleware cannot import handlers (cycle), so the lock lives here in the
// api package where both dependencies are legal.
func TestErrorCodeConstantsLocked(t *testing.T) {
	if handlers.CodeInternal != "internal_error" {
		t.Errorf("handlers.CodeInternal = %q, want %q", handlers.CodeInternal, "internal_error")
	}
	if handlers.CodeMethodNotAllowed != "method_not_allowed" {
		t.Errorf("handlers.CodeMethodNotAllowed = %q, want %q", handlers.CodeMethodNotAllowed, "method_not_allowed")
	}
	if handlers.CodeNotFound != "not_found" {
		t.Errorf("handlers.CodeNotFound = %q, want %q", handlers.CodeNotFound, "not_found")
	}
}

// TestHealthzServiceDown verifies the degraded contract: when the database
// ping fails the endpoint returns 503 and carries the x-ms-error-code
// header so probes and load balancers can treat the service as unavailable.
// The lazy pool points at 127.0.0.1:1 where nothing listens, so the ping is
// guaranteed to fail.
func TestHealthzServiceDown(t *testing.T) {
	r := NewRouter(testRouterPool(t), testRouterCipher(t))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if got := w.Header().Get(middleware.XMSErrorCodeHeader); got != handlers.CodeServiceDown {
		t.Errorf("x-ms-error-code header = %q, want %q", got, handlers.CodeServiceDown)
	}
}
