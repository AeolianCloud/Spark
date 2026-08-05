package swagger

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// TestDocsHandlerPage 验证 GET /docs 渲染 Swagger UI 页面：
// 返回 200、Content-Type 为 text/html，且 HTML 包含 swagger-ui 关键
// 标记（#swagger-ui 挂载点）与指向 /openapi.yaml 的 swaggerJsonUrl。
func TestDocsHandlerPage(t *testing.T) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/docs", nil)
	DocsHandler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := w.Body.String()
	for _, marker := range []string{"#swagger-ui", "swagger-ui-bundle.js", "/openapi.yaml"} {
		if !strings.Contains(body, marker) {
			t.Errorf("body 缺少关键标记 %q", marker)
		}
	}
}

// TestDocsHandlerStaticAssets 验证 swagger-ui 静态资源按 /docs/* 提供：
// 缺少这些资源时 UI 页面无法正常工作（js/css 404）。除 200 与非空 body
// 外，还断言 Content-Type 匹配实际资源类型（css 为 text/css、js 为
// application/javascript），防止资源内容被错误替换。
func TestDocsHandlerStaticAssets(t *testing.T) {
	for _, tc := range []struct {
		path string
		ct   string
	}{
		{"/docs/swagger-ui.css", "text/css"},
		{"/docs/swagger-ui-bundle.js", "application/javascript"},
		{"/docs/swagger-ui-standalone-preset.js", "application/javascript"},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		DocsHandler().ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want %d", tc.path, w.Code, http.StatusOK)
		}
		if w.Body.Len() == 0 {
			t.Errorf("GET %s: body 为空", tc.path)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, tc.ct) {
			t.Errorf("GET %s: Content-Type = %q, want 包含 %q", tc.path, ct, tc.ct)
		}
	}
}

// TestOpenAPIYAMLCopyConsistency 验证契约副本同步：包内 embed 的
// openapi.yaml（API 实际对外输出的契约）必须与仓库根目录的
// docs/openapi.yaml 字节一致。go test 工作目录为包目录，故用相对路径
// ../../docs/openapi.yaml 读取根目录副本。两者不一致说明契约变更后未
// 同步到 api/swagger 包，需要运行同步命令重新生成内嵌副本。
func TestOpenAPIYAMLCopyConsistency(t *testing.T) {
	rootCopy, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatalf("读取 docs/openapi.yaml 失败: %v", err)
	}
	if !bytes.Equal(rootCopy, openapiYAML) {
		t.Error("docs/openapi.yaml 与 api/swagger/openapi.yaml 不同步，请运行同步命令")
	}
}

// TestOpenAPIYAMLHandler 验证 GET /openapi.yaml 输出契约：
// 返回 200、Content-Type 为 application/yaml，body 是可解析的 YAML
// 且声明 openapi 3.0.3、info.title 为契约标题。
func TestOpenAPIYAMLHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)

	OpenAPIYAML(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/yaml" {
		t.Errorf("Content-Type = %q, want application/yaml", ct)
	}
	var doc struct {
		OpenAPI string `yaml:"openapi"`
		Info    struct {
			Title string `yaml:"title"`
		} `yaml:"info"`
	}
	if err := yaml.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("body 不是合法 YAML: %v", err)
	}
	if doc.OpenAPI != "3.0.3" {
		t.Errorf("openapi 版本 = %q, want 3.0.3", doc.OpenAPI)
	}
	if doc.Info.Title != "Spark PVE Management API" {
		t.Errorf("info.title = %q, want %q", doc.Info.Title, "Spark PVE Management API")
	}
}
