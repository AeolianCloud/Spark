package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// echoRID builds a minimal router with RequestID() that reports the id seen
// by the handler through a header, so tests can observe what was accepted
// or generated.
func echoRID(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID())
	r.GET("/", func(c *gin.Context) {
		c.Header("X-RID-Seen", c.GetString(RequestIDKey))
		c.Status(http.StatusNoContent)
	})
	return r
}

func TestRequestIDAcceptsValidClientID(t *testing.T) {
	// A well-formed client id is passed through untouched.
	const rid = "req-123.abc_def"
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(XRequestIDHeader, rid)
	echoRID(t).ServeHTTP(w, req)

	if got := w.Header().Get(XRequestIDHeader); got != rid {
		t.Errorf("response X-Request-ID = %q, want passthrough %q", got, rid)
	}
	if got := w.Header().Get("X-RID-Seen"); got != rid {
		t.Errorf("context request id = %q, want %q", got, rid)
	}
}

func TestRequestIDAcceptsExactlyMaxLength(t *testing.T) {
	// 恰好 maxRequestIDLen 个字符是合法的，必须原样透传。
	rid := strings.Repeat("a", maxRequestIDLen)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(XRequestIDHeader, rid)
	echoRID(t).ServeHTTP(w, req)

	if got := w.Header().Get(XRequestIDHeader); got != rid {
		t.Errorf("response X-Request-ID = %q, want passthrough %q", got, rid)
	}
	if seen := w.Header().Get("X-RID-Seen"); seen != rid {
		t.Errorf("context request id = %q, want %q", seen, rid)
	}
}

func TestRequestIDRejectsInvalidClientID(t *testing.T) {
	// Ids outside the documented charset or longer than maxRequestIDLen are
	// discarded and replaced by a fresh uuid, keeping logs and the response
	// header free of client-controlled garbage.
	tests := []struct {
		name string
		rid  string
	}{
		{name: "semicolon", rid: "abc;drop table"},
		{name: "space", rid: "abc def"},
		{name: "control character", rid: "abc\nxyz"},
		{name: "non-ascii", rid: "请求id"},
		{name: "tilde", rid: "abc~def"},
		{name: "colon", rid: "abc:def"},
		{name: "at sign", rid: "abc@def"},
		{name: "overlong", rid: strings.Repeat("a", maxRequestIDLen+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set(XRequestIDHeader, tt.rid)
			echoRID(t).ServeHTTP(w, req)

			got := w.Header().Get(XRequestIDHeader)
			if got == tt.rid {
				t.Errorf("invalid id %q was passed through", tt.rid)
			}
			if _, err := uuid.Parse(got); err != nil {
				t.Errorf("generated id %q is not a uuid: %v", got, err)
			}
			if seen := w.Header().Get("X-RID-Seen"); seen != got {
				t.Errorf("context request id = %q, want %q", seen, got)
			}
		})
	}
}

func TestRequestIDGeneratesWhenMissing(t *testing.T) {
	// Without a client-supplied id a uuid is generated and echoed back.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	echoRID(t).ServeHTTP(w, req)

	got := w.Header().Get(XRequestIDHeader)
	if got == "" {
		t.Fatal("missing X-Request-ID header")
	}
	if _, err := uuid.Parse(got); err != nil {
		t.Errorf("generated id %q is not a uuid: %v", got, err)
	}
}

func TestErrCodeInternalContract(t *testing.T) {
	// errCodeInternal 是文档化 API 契约的一部分（docs/api-errors.md），
	// 必须与 handlers 包的 CodeInternal 保持一致；handlers 侧的同值断言
	// 在 api/router_test.go 中（middleware 不能 import handlers，否则成环）。
	if errCodeInternal != "internal_error" {
		t.Errorf("errCodeInternal = %q, want %q", errCodeInternal, "internal_error")
	}
}

func TestRecoverySetsErrorCodeHeader(t *testing.T) {
	// A panic must yield the unified 500 response including the
	// x-ms-error-code header and the matching body code.
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Recovery())
	r.GET("/panic", func(c *gin.Context) { panic("boom") })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
	if got := w.Header().Get(XMSErrorCodeHeader); got != errCodeInternal {
		t.Errorf("x-ms-error-code header = %q, want %q", got, errCodeInternal)
	}

	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != errCodeInternal {
		t.Errorf("body error code = %q, want %q", body.Error.Code, errCodeInternal)
	}
	if body.Error.Code != w.Header().Get(XMSErrorCodeHeader) {
		t.Errorf("body error code = %q, want header value %q",
			body.Error.Code, w.Header().Get(XMSErrorCodeHeader))
	}
	if body.Error.Message == "" {
		t.Error("body error message is empty")
	}
}
