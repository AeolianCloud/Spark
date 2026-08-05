// Package api wires the gin engine: middleware, base routes and the
// registration points where each feature group appends its own routes.
package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark/api/handlers"
	"spark/api/middleware"
	"spark/crypto"
	"spark/pve"
	"spark/repository"
	"spark/service"
)

// routerOptions carries the optional construction overrides of NewRouter.
type routerOptions struct {
	// vmClientFactory replaces the PVE client factory of the VM service when
	// set; nil keeps the default (https://{host}:8006/api2/json).
	vmClientFactory func(host, apiUser, apiTokenSecret string) *pve.Client
}

// RouterOption customizes router construction. It is primarily a test seam:
// production deployments keep NewRouter's defaults.
type RouterOption func(o *routerOptions)

// WithVMClientFactory overrides the PVE client factory used by the VM
// service, so tests can point the provisioning chain, lifecycle calls and
// pass-through queries at fake PVE servers (the router otherwise has no way
// to know a non-default base URL).
func WithVMClientFactory(fn func(host, apiUser, apiTokenSecret string) *pve.Client) RouterOption {
	return func(o *routerOptions) { o.vmClientFactory = fn }
}

// NewRouter builds the gin engine with middleware and all routes.
func NewRouter(pool *pgxpool.Pool, cipher *crypto.Cipher, opts ...RouterOption) *gin.Engine {
	r := gin.New()
	// Trust no proxies: there is no reverse proxy in front of the API today,
	// and by default gin honors X-Forwarded-For and similar headers, letting
	// clients spoof their remote address. If a reverse proxy is introduced
	// later, configure its trusted CIDR via r.SetTrustedProxies(...).
	r.SetTrustedProxies(nil)
	// 开启后，路径存在但方法不匹配的请求才会进入 NoMethod handler
	//（否则 gin 一律按 404 处理，无法返回带 Allow 头的 405）。
	r.HandleMethodNotAllowed = true
	r.Use(middleware.RequestID(), middleware.Logger(), middleware.Recovery())

	health := handlers.NewHealthHandler(pool)
	r.GET("/healthz", health.Healthz)

	options := routerOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	registerRoutes(r, pool, cipher, options)
	registerErrorRoutes(r)

	return r
}

// registerErrorRoutes installs the unified 404/405 responses for paths and
// methods the route table does not serve. Both render the same
// {"error":{"code","message"}} shape as handlers.Handler and carry the
// x-ms-error-code header, so the error contract covers the whole API
// surface, not just handler-invoked routes.
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

// allowedMethods collects the HTTP methods registered for the request path
// by matching it segment-for-segment against the route table. Route patterns
// may contain :param segments, which match any single segment, so requests
// against /zones/42/nodes find the GET/POST on /zones/:zone_id/nodes.
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

// registerRoutes is the single place where feature route groups are mounted
// and where the shared repositories/services are constructed for the router.
func registerRoutes(r *gin.Engine, pool *pgxpool.Pool, cipher *crypto.Cipher, options routerOptions) {
	// Shared data access. NewZoneRepository/NewIPPoolRepository accept the
	// pgxQuerier subset, which *pgxpool.Pool satisfies.
	zoneRepo := repository.NewZoneRepository(pool)
	nodeRepo := repository.NewNodeRepository(pool)
	ipPoolRepo := repository.NewIPPoolRepository(pool)
	storageTypeRepo := repository.NewStorageTypeRepository(pool)
	imageRepo := repository.NewImageRepository(pool)

	// Feature services; each pair of repo/service is self-contained, so
	// there is no dependency cycle between groups.
	zoneSvc := service.NewZoneService(zoneRepo, nodeRepo)
	ipPoolSvc := service.NewIPPoolService(ipPoolRepo, zoneRepo, nodeRepo)
	storageTypeSvc := service.NewStorageTypeService(storageTypeRepo)
	imageSvc := service.NewImageService(imageRepo)

	// ===== zones handler (task 4.1) + pve nodes handler (task 4.2) =====
	// RegisterZonesRoutes takes both the /zones and /nodes groups: node
	// routes mostly live under /zones/:zone_id/nodes, but PUT /nodes/:id
	// sits outside /zones.
	handlers.RegisterZonesRoutes(r.Group("/zones"), r.Group("/nodes"), zoneSvc)

	// ===== ip pool handler (task 5.x) =====
	handlers.RegisterIPPoolsRoutes(r.Group("/ip-pools"), ipPoolSvc)

	// ===== storage types handler (task 6.1) =====
	handlers.RegisterStorageTypesRoutes(r.Group("/storage-types"), storageTypeSvc)

	// ===== images handler (task 6.2) =====
	handlers.RegisterImagesRoutes(r.Group("/images"), imageSvc)

	// ===== vm lifecycle handler (task 7.x) =====
	// NewVMService receives the pool as its transaction entry point: the
	// IP-allocation/destroy transaction orchestration lives in the service
	// layer (migration 0002 conventions).
	vmRepo := repository.NewVMRepository(pool)
	vmSvc := service.NewVMService(pool, vmRepo, ipPoolRepo, zoneRepo, nodeRepo, imageRepo, storageTypeRepo, cipher)
	if options.vmClientFactory != nil {
		vmSvc.SetClientFactory(options.vmClientFactory)
	}
	handlers.RegisterVMsRoutes(r.Group("/vms"), vmSvc)
}
