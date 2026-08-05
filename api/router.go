// Package api wires the gin engine: middleware, base routes and the
// registration points where each feature group appends its own routes.
package api

import (
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
	r.Use(middleware.RequestID(), middleware.Logger(), middleware.Recovery())

	health := handlers.NewHealthHandler(pool)
	r.GET("/healthz", health.Healthz)

	options := routerOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	registerRoutes(r, pool, cipher, options)

	return r
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
