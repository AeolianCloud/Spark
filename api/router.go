// Package api 组装 gin 引擎：中间件、基础路由，以及各功能分组
// 挂载自身路由的注册点。
package api

import (
	"fmt"
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
	// imageClientFactory 在设置时替换镜像服务的 PVE 客户端工厂；
	// 为 nil 时保持默认值（https://{host}:{port}/api2/json，port 取
	// 节点持久化的端口）。
	imageClientFactory func(host string, port int, apiUser, apiTokenSecret string) *pve.Client
	// imageDownloadHostAllowlist 是镜像下载源域名白名单（SSRF 面控制）：
	// download_url 的 host 必须精确命中才受理镜像创建与下载；空列表语义
	// 为拒绝所有下载。为 nil 时镜像服务保持其内置默认白名单（与
	// config.Default 一致）。
	imageDownloadHostAllowlist []string
	// jwtSecret 是 JWT 签发与校验的 HMAC-SHA256 密钥（config
	// auth.jwt_secret）；为空时 auth 服务构造失败并 panic（config 层
	// 已保证必填且足够长，见 G1）。
	jwtSecret string
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

// WithImageClientFactory 覆盖镜像服务使用的 PVE 客户端工厂，使测试可以将
// 镜像存在性扫描与下载编排指向模拟 PVE 服务器。
func WithImageClientFactory(fn func(host string, port int, apiUser, apiTokenSecret string) *pve.Client) RouterOption {
	return func(o *routerOptions) { o.imageClientFactory = fn }
}

// WithImageDownloadHostAllowlist 覆盖镜像服务用于下载受理校验的域名白名单
// （镜像 download_url 的 host 必须精确命中，忽略端口；空列表拒绝所有下载）。
// 生产部署由 cmd/server 传入 config.Images.DownloadHostAllowlist。
func WithImageDownloadHostAllowlist(hosts []string) RouterOption {
	return func(o *routerOptions) { o.imageDownloadHostAllowlist = hosts }
}

// WithJWTSecret 注入 JWT 签发与校验密钥（config auth.jwt_secret）。
// 生产部署由 cmd/server 传入；测试注入固定密钥以签发测试令牌。
// 密钥为空时 registerRoutes 会 panic（config 层已保证必填且足够长）。
func WithJWTSecret(secret string) RouterOption {
	return func(o *routerOptions) { o.jwtSecret = secret }
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

	// ===== 认证（task 3.x）与鉴权分层（task 4.3，设计 D4） =====
	// auth 仓储只承担登录/鉴权的最小只读查询（任务 5.x 将新增完整
	// User 仓储做 CRUD）。
	authRepo := repository.NewAuthRepository(pool)
	authSvc, err := service.NewAuthService([]byte(options.jwtSecret), authRepo)
	if err != nil {
		// config.validate 已强制 jwt_secret 非空且足够长（G1），密钥
		// 为空属启动期配置错误，panic 由 middleware.Recovery 兜底。
		panic(fmt.Errorf("auth service: %w", err))
	}
	// 公开组：双登录入口，不加鉴权。
	handlers.RegisterAuthRoutes(r.Group("/auth"), authSvc)

	// 受保护组：全部业务路由挂 requireAuth——解析 Bearer JWT 并按角色
	// 查库校验身份有效（设计 D4），身份注入 gin.Context 供 handler 分流。
	protected := r.Group("", middleware.RequireAuth([]byte(options.jwtSecret), authRepo))

	// 功能服务；每对 repo/service 自成一体，因此各组之间不存在依赖环。
	zoneSvc := service.NewZoneService(zoneRepo, nodeRepo)
	ipPoolSvc := service.NewIPPoolService(ipPoolRepo, zoneRepo, nodeRepo)
	storageTypeSvc := service.NewStorageTypeService(storageTypeRepo)
	imageOpRepo := repository.NewImageOperationRepository(pool)
	imageSvc := service.NewImageService(imageRepo, nodeRepo, imageOpRepo)
	if options.imageClientFactory != nil {
		imageSvc.SetClientFactory(options.imageClientFactory)
	}
	if options.imageDownloadHostAllowlist != nil {
		imageSvc.SetDownloadHostAllowlist(options.imageDownloadHostAllowlist)
	}

	// 管理员专用挂载点（设计 D6）：任务 5.x 的用户 CRUD 路由将挂在这里，
	// 如 protected.Group("", middleware.RequireAdmin()).Group("/users")
	// ——user 令牌访问返回 403 forbidden，仅管理员令牌放行。

	// ===== zones 处理器（task 4.1）+ pve nodes 处理器（task 4.2） =====
	// RegisterZonesRoutes 接收 /zones 和 /nodes 两个分组：node 路由大多
	// 位于 /zones/:zone_id/nodes 之下，但 PUT /nodes/:id 位于 /zones 之外。
	handlers.RegisterZonesRoutes(protected.Group("/zones"), protected.Group("/nodes"), zoneSvc)

	// ===== node 实时状态处理器（task 3.x） =====
	// RegisterNodeStatusRoutes 挂在独立的 /nodes 分组实例上（与
	// RegisterZonesRoutes 的分组互不干扰，路由可并存）：
	// GET /nodes/:id/status 实时拉取 PVE 状态并聚合返回。
	nodeStatusSvc := service.NewNodeStatusService(nodeRepo)
	handlers.RegisterNodeStatusRoutes(protected.Group("/nodes"), nodeStatusSvc)

	// ===== ip pool 处理器（task 5.x） =====
	handlers.RegisterIPPoolsRoutes(protected.Group("/ip-pools"), ipPoolSvc)

	// ===== storage types 处理器（task 6.1） =====
	handlers.RegisterStorageTypesRoutes(protected.Group("/storage-types"), storageTypeSvc)

	// ===== images 处理器（task 6.2） =====
	handlers.RegisterImagesRoutes(protected.Group("/images"), imageSvc)

	// ===== vm 生命周期处理器（task 7.x） =====
	// NewVMService 将 pool 作为其事务入口点：IP 分配/销毁的事务编排
	// 位于 service 层（migration 0002 约定）。
	vmRepo := repository.NewVMRepository(pool)
	opRepo := repository.NewVMOperationRepository(pool)
	vmSvc := service.NewVMService(pool, vmRepo, opRepo, ipPoolRepo, zoneRepo, nodeRepo, imageRepo, storageTypeRepo, cipher)
	if options.vmClientFactory != nil {
		vmSvc.SetClientFactory(options.vmClientFactory)
	}
	handlers.RegisterVMsRoutes(protected.Group("/vms"), vmSvc)
}
