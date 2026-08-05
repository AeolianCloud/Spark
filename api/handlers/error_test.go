package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"spark/api/middleware"
)

// errorBody mirrors the unified error payload {"error":{"code","message"}}.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// TestHandlerErrorResponseContract is a smoke test of the Handler wrapper:
// it drives a minimal gin engine through httptest and asserts the unified
// error contract — HTTP status, body {"error":{"code","message"}} and the
// x-ms-error-code response header mirroring the body code.
func TestHandlerErrorResponseContract(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		fn         func(c *gin.Context) error
		wantStatus int
		wantCode   string
	}{
		{
			name: "api error carries its code in body and header",
			fn: func(c *gin.Context) error {
				return ErrNotFound("vm 42 not found")
			},
			wantStatus: http.StatusNotFound,
			wantCode:   CodeNotFound,
		},
		{
			name: "non-api error becomes a generic internal error",
			fn: func(c *gin.Context) error {
				return errors.New("boom")
			},
			wantStatus: http.StatusInternalServerError,
			wantCode:   CodeInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := gin.New()
			r.GET("/err", Handler(tt.fn))

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/err", nil)
			r.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			if got := w.Header().Get(middleware.XMSErrorCodeHeader); got != tt.wantCode {
				t.Errorf("x-ms-error-code header = %q, want %q", got, tt.wantCode)
			}

			var body errorBody
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Error.Code != tt.wantCode {
				t.Errorf("body error code = %q, want %q", body.Error.Code, tt.wantCode)
			}
			if body.Error.Message == "" {
				t.Error("body error message is empty")
			}
		})
	}
}
