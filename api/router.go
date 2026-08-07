// Package api 组装 gin 引擎：中间件、基础路由，以及各功能分组
// 挂载自身路由的注册点。
package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark/api/handlers"
	"spark/api/middleware"
	"spark/api/swagger"
	"spark/crypto"
	"spark/pve"
	"spark/repository"
	"spark/service"
)

// routerOptions 携带 NewRouter 的可选构造覆盖项。
type routerOptions struct {
	// vmClientFactory 在设置时替换 VM 服务的 PVE 客户端工厂；
	// 为 nil 时保持默认值（https://{host}:{port}/api2/json，port 取
	// 节点持久化的端口）。
	vmClientFactory func(host string, port int, apiUser, apiTokenSecret string) *pve.Client
}

// RouterOption 定制路由的构造过程。它主要是一个测试接缝：
// 生产部署保持 NewRouter 的默认行为。
type RouterOption func(o *routerOptions)

// WithVMClientFactory 覆盖 VM 服务使用的 PVE 客户端工厂，使测试可以将
// 供给链路、生命周期调用和透传查询指向模拟 PVE 服务器（否则路由没有
// 途径得知非默认的 base URL）。
func WithVMClientFactory(fn func(host string, port int, apiUser, apiTokenSecret string) *pve.Client) RouterOption {
	return func(o *routerOptions) { o.vmClientFactory = fn }
}

// NewRouter 构建带中间件和全部路由的 gin 引擎。
func NewRouter(pool *pgxpool.Pool, cipher *crypto.Cipher, opts ...RouterOption) *gin.Engine {
	r := gin.New()
	// 不信任任何代理：目前 API 前面没有反向代理，而 gin 默认信任
	// X-Forwarded-For 等头，会让客户端伪造自己的远程地址。如果以后引入
	// 反向代理，请通过 r.SetTrustedProxies(...) 配置其可信 CIDR。
	r.SetTrustedProxies(nil)
	// 开启后，路径存在但方法不匹配的请求才会进入 NoMethod handler
	//（否则 gin 一律按 404 处理，无法返回带 Allow 头的 405）。
	r.HandleMethodNotAllowed = true
	r.Use(middleware.RequestID(), middleware.Logger(), middleware.Recovery())

	health := handlers.NewHealthHandler(pool)
	r.GET("/healthz", health.Healthz)

	registerDocsRoutes(r)

	options := routerOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	registerRoutes(r, pool, cipher, options)
	registerErrorRoutes(r)

	return r
}

// registerDocsRoutes 挂载 API 契约的在线浏览路由：
//   - GET /docs 渲染 Swagger UI 页面；swgui 处理器同时按 /docs/* 提供
//     swagger-ui 的静态资源（js/css），因此挂两条路由，一条精确匹配
//     页面路径、一条通配静态资源。注意：/docs 子树内未匹配的路径
//     （如缺失的静态资源）由 swgui 内部返回其 HTML 404 页面，不套用
//     统一错误契约（registerErrorRoutes 的 {"error":...} 结构），这是
//     文档路由的特例，其余 API 表面一律遵守统一错误契约。
//   - GET /openapi.yaml 输出 OpenAPI 契约内容（api/swagger 包内嵌的
//     docs/openapi.yaml 字节副本）。
//
// 这两条路由不写入契约本身（docs/openapi.yaml 的 paths），避免契约自指。
func registerDocsRoutes(r *gin.Engine) {
	docsHandler := swagger.DocsHandler()
	r.GET("/docs", gin.WrapH(docsHandler))
	r.GET("/docs/*any", gin.WrapH(docsHandler))
	r.GET("/openapi.yaml", swagger.OpenAPIYAML)
}

// registerErrorRoutes 为路由表未服务的路径和方法安装统一的 404/405 响应。
// 两者都渲染与 handlers.Handler 相同的 {"error":{"code","message"}} 结构，
// 并携带 x-ms-error-code 头，使错误契约覆盖整个 API 表面，
// 而不仅仅是处理器调用的路由。
func registerErrorRoutes(r *gin.Engine) {
	r.NoRoute(func(c *gin.Context) {
		c.Header(middleware.XMSErrorCodeHeader, handlers.CodeNotFound)
		c.JSON(http.StatusNotFound, gin.H{
			"error": handlers.ErrNotFound("resource not found"),
		})
	})
	r.NoMethod(func(c *gin.Context) {
		c.Header(middleware.XMSErrorCodeHeader, handlers.CodeMethodNotAllowed)
		// 405 应携带 Allow 头列出该路径允许的方法（REST 最佳实践），
		// 从注册的路由表中按路径匹配收集。
		if allow := allowedMethods(r, c.Request.URL.Path); allow != "" {
			c.Header("Allow", allow)
		}
		c.JSON(http.StatusMethodNotAllowed, gin.H{
			"error": handlers.NewError(http.StatusMethodNotAllowed,
				handlers.CodeMethodNotAllowed, "method not allowed"),
		})
	})
}

// allowedMethods 通过将请求路径与路由表逐段匹配，收集该路径上注册的
// HTTP 方法。路由模式可能包含 :param 段，它匹配任意单个段，因此对
// /zones/42/nodes 的请求能找到 /zones/:zone_id/nodes 上的 GET/POST。
func allowedMethods(r *gin.Engine, path string) string {
	reqParts := strings.Split(strings.Trim(path, "/"), "/")
	set := make(map[string]bool)
	for _, route := range r.Routes() {
		routeParts := strings.Split(strings.Trim(route.Path, "/"), "/")
		if len(routeParts) != len(reqParts) {
			continue
		}
		matched := true
		for i := range reqParts {
			part := routeParts[i]
			if part != reqParts[i] && !strings.HasPrefix(part, ":") {
				matched = false
				break
			}
		}
		if matched {
			set[route.Method] = true
		}
	}
	if len(set) == 0 {
		return ""
	}
	methods := make([]string, 0, len(set))
	for m := range set {
		methods = append(methods, m)
	}
	sort.Strings(methods)
	return strings.Join(methods, ", ")
}

// registerRoutes 是功能路由分组挂载、以及为路由构建共享
// repository/service 的唯一位置。
func registerRoutes(r *gin.Engine, pool *pgxpool.Pool, cipher *crypto.Cipher, options routerOptions) {
	// 共享数据访问。NewZoneRepository/NewIPPoolRepository 接受
	// pgxQuerier 子集，*pgxpool.Pool 满足该接口。
	zoneRepo := repository.NewZoneRepository(pool)
	nodeRepo := repository.NewNodeRepository(pool)
	ipPoolRepo := repository.NewIPPoolRepository(pool)
	storageTypeRepo := repository.NewStorageTypeRepository(pool)
	imageRepo := repository.NewImageRepository(pool)

	// 功能服务；每对 repo/service 自成一体，因此各组之间不存在依赖环。
	zoneSvc := service.NewZoneService(zoneRepo, nodeRepo)
	ipPoolSvc := service.NewIPPoolService(ipPoolRepo, zoneRepo, nodeRepo)
	storageTypeSvc := service.NewStorageTypeService(storageTypeRepo)
	imageSvc := service.NewImageService(imageRepo)

	// ===== zones 处理器（task 4.1）+ pve nodes 处理器（task 4.2） =====
	// RegisterZonesRoutes 接收 /zones 和 /nodes 两个分组：node 路由大多
	// 位于 /zones/:zone_id/nodes 之下，但 PUT /nodes/:id 位于 /zones 之外。
	handlers.RegisterZonesRoutes(r.Group("/zones"), r.Group("/nodes"), zoneSvc)

	// ===== ip pool 处理器（task 5.x） =====
	handlers.RegisterIPPoolsRoutes(r.Group("/ip-pools"), ipPoolSvc)

	// ===== storage types 处理器（task 6.1） =====
	handlers.RegisterStorageTypesRoutes(r.Group("/storage-types"), storageTypeSvc)

	// ===== images 处理器（task 6.2） =====
	handlers.RegisterImagesRoutes(r.Group("/images"), imageSvc)

	// ===== vm 生命周期处理器（task 7.x） =====
	// NewVMService 将 pool 作为其事务入口点：IP 分配/销毁的事务编排
	// 位于 service 层（migration 0002 约定）。
	vmRepo := repository.NewVMRepository(pool)
	opRepo := repository.NewVMOperationRepository(pool)
	vmSvc := service.NewVMService(pool, vmRepo, opRepo, ipPoolRepo, zoneRepo, nodeRepo, imageRepo, storageTypeRepo, cipher)
	if options.vmClientFactory != nil {
		vmSvc.SetClientFactory(options.vmClientFactory)
	}
	handlers.RegisterVMsRoutes(r.Group("/vms"), vmSvc)
}
