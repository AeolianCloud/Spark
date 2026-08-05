package swagger

import (
	_ "embed"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/swaggest/swgui"
	"github.com/swaggest/swgui/v5emb"
)

//go:embed openapi.yaml
var openapiYAML []byte

// DocsHandler 构建 Swagger UI 页面处理器（github.com/swaggest/swgui
// v5emb 包，纯 Go embed swagger-ui 静态资源，v1.8.9 对应 Swagger UI
// 5.32.8）。BasePath 为 /docs：页面 HTML 由 GET /docs 渲染，swagger-ui
// 的 js/css 静态资源按 /docs/* 路径提供。swaggerJsonUrl 指向本服务自身
// 输出的 /openapi.yaml，页面加载时从该地址拉取契约（相对 UI 的同源路径）。
// SettingsUI 中 defaultModelsExpandDepth 设为 "1"：swgui 默认用 "-1"
// 隐藏 Schemas 区块，改为 "1" 展开并展示全部端点涉及的 schema（与契约
// 规格"可浏览全部端点与 schema"一致）。
func DocsHandler() http.Handler {
	return v5emb.NewHandlerWithConfig(swgui.Config{
		Title:       "Spark PVE Management API",
		SwaggerJSON: "/openapi.yaml",
		BasePath:    "/docs",
		ShowTopBar:  true,
		SettingsUI: map[string]string{
			"defaultModelsExpandDepth": "1",
		},
	})
}

// OpenAPIYAML 输出内嵌的 OpenAPI 契约内容，Content-Type 为
// application/yaml。契约副本与 docs/openapi.yaml 字节一致（见
// doc.go 的同步说明）。
func OpenAPIYAML(c *gin.Context) {
	c.Data(http.StatusOK, "application/yaml", openapiYAML)
}
