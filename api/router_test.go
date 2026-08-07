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
	"gopkg.in/yaml.v3"

	"spark/api/handlers"
	"spark/api/middleware"
	"spark/config"
	"spark/crypto"
)

// testRouterPool 构建一个从不打开连接的懒加载 pool，
// 足以用于不触碰数据库的路由层测试。
func testRouterPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://user:pass@127.0.0.1:1/db")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// testRouterCipher 用确定的 32 字节密钥构建 cipher。
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

// TestRouterRegistersAllRoutes 用懒加载 pool（不打开任何连接）构建完整
// 路由并断言完整路由表。gin 在路由注册冲突时会 panic，因此仅构建路由
// 本身就是冲突检查；断言则固定了预期路径，包括 batch-7 的 /vms 分组。
func TestRouterRegistersAllRoutes(t *testing.T) {
	pool := testRouterPool(t)

	r := NewRouter(pool, testRouterCipher(t))

	want := []string{
		"GET /healthz",
		"GET /docs",
		"GET /docs/*any",
		"GET /openapi.yaml",
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
		"GET /images/:id",
		"GET /images/:id/nodes-status",
		"POST /images/:id/download",
		"GET /images/:id/operations",
		"POST /vms",
		"GET /vms",
		"POST /vms/import",
		"GET /vms/:id",
		"POST /vms/:id/start",
		"POST /vms/:id/stop",
		"POST /vms/:id/restart",
		"GET /vms/:id/operations",
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

// errorBody 镜像统一的错误负载 {"error":{"code","message"}}。
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// TestRouterUnmatchedPathAndMethod 验证统一错误契约覆盖整个 API 表面：
// 未知路径返回 404 结构，已知路径配未注册方法返回 405 并带 Allow 头。
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

// TestDocsRoutes 走完整路由栈验证契约在线浏览端点：GET /docs 返回
// Swagger UI 页面（含 swagger-ui 关键标记），GET /openapi.yaml 返回
// 200 且 Content-Type 为 application/yaml、body 可解析为 YAML 并声明
// openapi 3.0.3。这两条路由刻意不写入契约本身（docs/openapi.yaml），
// 因此断言的是实际挂载行为而非契约内容。
func TestDocsRoutes(t *testing.T) {
	r := NewRouter(testRouterPool(t), testRouterCipher(t))

	t.Run("GET /docs renders swagger ui page", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/docs", nil)
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if !strings.Contains(w.Body.String(), "#swagger-ui") {
			t.Error("body 缺少 swagger-ui 挂载点标记 #swagger-ui")
		}
	})

	t.Run("GET /openapi.yaml serves contract", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if got := w.Header().Get("Content-Type"); got != "application/yaml" {
			t.Errorf("Content-Type = %q, want application/yaml", got)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Fatalf("body 不是合法 YAML: %v", err)
		}
		if doc["openapi"] != "3.0.3" {
			t.Errorf("openapi 版本 = %v, want 3.0.3", doc["openapi"])
		}
	})
}

// TestErrorCodeConstantsLocked 守护跨包重复的 internal-error 错误码：
// handlers.CodeInternal 与 middleware.errCodeInternal（在
// middleware_test.go 中断言）都必须等于文档化的值。
// middleware 不能 import handlers（成环），因此该锁定断言放在
// api 包中，那里两个依赖都是合法的。
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

// TestHealthzServiceDown 验证 degraded 契约：数据库 ping 失败时
// 端点返回 503 并携带 x-ms-error-code 头，使探活器与负载均衡可将其
// 视为服务不可用。懒加载 pool 指向无人监听的 127.0.0.1:1，
// 因此 ping 必然失败。
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
