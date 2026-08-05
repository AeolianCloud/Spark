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

// echoRID 构建带 RequestID() 的最小路由，处理器通过一个响应头
// 报告它看到的 id，使测试能观察到被接受或生成的 id。
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
	// 格式合法的客户端 id 被原样透传。
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
	// 超出文档化字符集或长于 maxRequestIDLen 的 id 会被丢弃并替换为
	// 全新的 uuid，使日志和响应头不包含客户端可控的垃圾数据。
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
	// 没有客户端提供的 id 时生成 uuid 并回显。
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
	// panic 必须产生统一的 500 响应，包括
	// x-ms-error-code 头及与之匹配的 body 错误码。
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
