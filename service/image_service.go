package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"spark/model"
	"spark/pve"
	"spark/repository"
)

const (
	// imageDownloadTimeout 限制单个节点的镜像下载编排（DownloadURL 受理 +
	// WaitTask 轮询）的总时长，作为后台 goroutine 的 context 超时（与
	// vmProvisionTimeout 同模式）。
	imageDownloadTimeout = 30 * time.Minute
	// maxImageOpErrorLen 限制 image_operations.error_message 的最大长度
	//（字符数），与 vm_operations 的错误消息截断约定对齐（1000 字符）：
	// 落库前截断保证永不触发 Postgres 的字符串超长错误（SQLSTATE 22001）。
	maxImageOpErrorLen = 1000
	// maxImageDownloadNodes 限制单次镜像下载请求可指定的 node_ids 个数，
	// 防止一次性对大量节点发起下载。
	maxImageDownloadNodes = 64
)

// defaultImageDownloadHosts 是 ImageService 内置的默认镜像下载源域名白名单，
// 与 config 包的默认值保持一致（镜像下载由 PVE 节点代发，属 SSRF 面控制）：
// NewImageService 未收到 SetDownloadHostAllowlist 覆盖时使用它，保证单测与
// 未显式配置的部署行为一致。生产环境通过 config.Images.DownloadHostAllowlist
// 注入；修改本常量时必须同步更新 config 包的对应默认值。
var defaultImageDownloadHosts = []string{
	"cloud.debian.org",
	"cloud-images.ubuntu.com",
	"cloud.centos.org",
	"download.cirros-cloud.net",
	"cloud-images.rockylinux.org",
}

// ImageNodeRepository 是 ImageService 依赖的节点数据访问层。
type ImageNodeRepository interface {
	GetNode(ctx context.Context, id int64) (*model.PVENode, error)
	ListEnabledNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error)
	// ListAllEnabledNodes 返回全部已启用节点（不按区域过滤），供不带区域
	// 的节点状态查询（GetImageNodeStatus）。
	ListAllEnabledNodes(ctx context.Context) ([]model.PVENode, error)
}

// ImageOperationRepository 是 ImageService 依赖的 image_operations 数据访问
// 层（镜像下载操作的审计记录，设计 D5）。
type ImageOperationRepository interface {
	CreateOperation(ctx context.Context, op model.ImageOperation) (*model.ImageOperation, error)
	// UpdateOperationResult 幂等地将操作记录更新为终态（success/failed）。
	UpdateOperationResult(ctx context.Context, id int64, result, errorMessage, upid string) error
	// ListOperationsByImage 按时间倒序分页返回指定镜像的下载操作记录及匹配
	// 总数。
	ListOperationsByImage(ctx context.Context, imageID int64, limit, offset int) ([]model.ImageOperation, int, error)
	// HasRunningOperation 报告指定镜像是否已有在指定节点上未终态
	//（result='running'）的下载操作记录，供受理下载前的幂等检查。
	HasRunningOperation(ctx context.Context, imageID, nodeID int64) (bool, error)
}

// ImageService 实现已注册云镜像的业务规则：元数据管理、以 PVE 为权威源的
// 节点存在性扫描（设计 D1）、下载编排（设计 D6）与下载历史查询。镜像在各
// 节点上的存在状态不落库，一律以 PVE 实时扫描为准。
type ImageService struct {
	repo     *repository.ImageRepository
	nodeRepo ImageNodeRepository
	opRepo   ImageOperationRepository
	// newClient 为节点构建 PVE 客户端（host/port/API 用户/token）；可注入，
	// 以便测试将扫描与下载调用指向假服务器。
	newClient func(host string, port int, apiUser, apiTokenSecret string) *pve.Client
	// downloadHostAllowlist 是镜像下载源域名白名单：download_url 的 host
	//（忽略端口，精确匹配）必须命中才受理创建与下载；空列表拒绝所有下载
	//（SSRF 面最小化）。默认取 defaultImageDownloadHosts（与 config 内置
	// 一致），生产通过 SetDownloadHostAllowlist 以配置覆盖。
	downloadHostAllowlist []string
}

// NewImageService 使用镜像仓库、节点仓库与操作仓库创建一个 ImageService。
func NewImageService(repo *repository.ImageRepository, nodeRepo ImageNodeRepository, opRepo ImageOperationRepository) *ImageService {
	return &ImageService{
		repo:     repo,
		nodeRepo: nodeRepo,
		opRepo:   opRepo,
		newClient: func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
			return pve.NewClient(host, apiUser, apiTokenSecret, pve.WithPort(port))
		},
		// 拷贝默认白名单（与 config.Default 一致），避免外部共享底层切片。
		downloadHostAllowlist: append([]string(nil), defaultImageDownloadHosts...),
	}
}

// SetClientFactory 替换用于所有节点交互（存在性扫描、下载编排）的 PVE
// 客户端工厂。默认工厂针对 https://{host}:{port}/api2/json（port 取节点
// 持久化的端口）构建客户端；覆盖它可以让调用方将服务指向不同的 base URL
// （测试、反向代理）。
func (s *ImageService) SetClientFactory(fn func(host string, port int, apiUser, apiTokenSecret string) *pve.Client) {
	if fn != nil {
		s.newClient = fn
	}
}

// SetDownloadHostAllowlist 覆盖镜像下载源域名白名单（Create 与 Download
// 受理校验共用）。nil/空列表语义为拒绝所有下载（SSRF 面最小化）；生产
// 部署由 config.Images.DownloadHostAllowlist 注入，未调用时保持内置默认
// 白名单（与 config.Default 一致）。
func (s *ImageService) SetDownloadHostAllowlist(hosts []string) {
	s.downloadHostAllowlist = append([]string(nil), hosts...)
}

// imageDownloadURLIssue 校验镜像下载地址并返回拒绝理由；返回空串表示
// 通过。download_url 的下载请求最终由 PVE 节点代发（SSRF 的受害方），本
// 服务是唯一可控的校验点：
//   - 非 http(s) 协议（file://、gopher:// 等）一律拒绝；
//   - http(s) 但缺失 host（如 "https://"）同样拒绝；
//   - host（u.Hostname()，忽略端口）必须精确命中下载源白名单
//     （downloadHostAllowlist），空白名单拒绝一切下载。
//
// 创建（Create）与受理下载（Download）两处共用本函数。
func (s *ImageService) imageDownloadURLIssue(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "image download_url must be an http(s) URL"
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "image download_url must be an http(s) URL"
	}
	for _, h := range s.downloadHostAllowlist {
		if u.Hostname() == h {
			return ""
		}
	}
	return fmt.Sprintf("image download_url host %q is not allowlisted", u.Hostname())
}

// validImageFileName 报告镜像 download_url 的文件名（imageFileName 的
// path.Base 结果）是否可作为镜像文件名：非空、非 "." / ".."、不含
// "/" 或 "\"（path.Base 语义下本应不会出现，仅作防御，防止把目录或
// 上级路径当作文件名交给 PVE 下载）。
func validImageFileName(imageURL string) bool {
	name := imageFileName(imageURL)
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\")
}

// imageFileName 返回镜像 download_url 的文件名（path.Base 语义，设计 D2）：
// 与扫描匹配（imageFileMatches）使用同一 basename，保证"下载出的文件名 =
// 扫描匹配的文件名"，URL 带查询串时也不会把查询串带进文件名。URL 解析
// 失败返回空串（Create/Download 已校验 URL 可解析，这里仅作防御）。
func imageFileName(imageURL string) string {
	u, err := url.Parse(imageURL)
	if err != nil {
		return ""
	}
	return path.Base(u.Path)
}

// imageFileMatches 判断镜像下载地址的文件名与存储内容条目名是否一致
// （设计 D2）：两者按 path.Base 语义比较 download_url 路径尾段。URL 解析
// 失败视为不匹配（管理员填写的 download_url 必须是完整可解析的 http(s)
// URL，Create 时已校验）。
func imageFileMatches(imageURL, name string) bool {
	return imageFileName(imageURL) == name
}

// Create 校验字段并持久化一个新的镜像。名称、默认用户与 download_url 均
// 不得为空；download_url 必须为可解析的 http(s) URL 且 host 命中下载源
// 白名单，文件名必须合法（该地址后续由 PVE 侧代发下载，协议与域名校验
// 在此处尽早暴露参数错误）。名称重复视为冲突。
func (s *ImageService) Create(ctx context.Context, name, defaultUser, downloadURL string) (*model.Image, error) {
	switch {
	case strings.TrimSpace(name) == "":
		return nil, badRequestf("image name is required")
	case strings.TrimSpace(defaultUser) == "":
		return nil, badRequestf("image default_user is required")
	case strings.TrimSpace(downloadURL) == "":
		return nil, badRequestf("image download_url is required")
	case !validImageFileName(downloadURL):
		return nil, badRequestf("image download_url must end with a valid image file name")
	}
	if issue := s.imageDownloadURLIssue(downloadURL); issue != "" {
		return nil, badRequestf("%s", issue)
	}
	img, err := s.repo.Create(ctx, strings.TrimSpace(name), strings.TrimSpace(defaultUser), strings.TrimSpace(downloadURL))
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, conflictf("image name %q already exists", name)
		}
		return nil, fmt.Errorf("create image: %w", err)
	}
	return img, nil
}

// Get 返回指定 id 的镜像，或返回 not_found 错误。
func (s *ImageService) Get(ctx context.Context, id int64) (*model.Image, error) {
	img, err := s.repo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("image %d not found", id)
		}
		return nil, fmt.Errorf("get image: %w", err)
	}
	return img, nil
}

// GetByName 返回指定名称的镜像，或返回 not_found 错误。
func (s *ImageService) GetByName(ctx context.Context, name string) (*model.Image, error) {
	img, err := s.repo.GetByName(ctx, name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("image %q not found", name)
		}
		return nil, fmt.Errorf("get image by name: %w", err)
	}
	return img, nil
}

// List 返回一页全部已注册镜像；total 是镜像总数，与分页无关。
func (s *ImageService) List(ctx context.Context, limit, offset int) ([]model.Image, int, error) {
	images, err := s.repo.ListPage(ctx, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list images: %w", err)
	}
	total, err := s.repo.Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list images: count: %w", err)
	}
	return images, total, nil
}

// NodeImageStatus 描述镜像在单个 PVE 节点上的存在状态（设计 D1/D2）：扫描
// 该节点 local/import 存储内容并与镜像 download_url 的文件名比对得出。
type NodeImageStatus struct {
	// NodeID 是本地 pve_nodes.id。
	NodeID int64 `json:"node_id"`
	// NodeName 是节点业务名（pve_nodes.name）。
	NodeName string `json:"node_name"`
	// PveName 是节点在 PVE 集群中的节点名（pve_nodes.pve_name）；为空时
	// 表示与 NodeName 相同（存量数据，见 nodeName helper）。
	PveName string `json:"pve_name"`
	// Downloaded 为该节点上是否已存在该镜像文件。
	Downloaded bool `json:"downloaded"`
	// VolID 是匹配到的卷 ID（如 local:import/debian.qcow2），未匹配或节点
	// 扫描失败时为空。
	VolID string `json:"volid"`
}

// ImageZoneItem 是 ListImagesByZone 返回的条目：镜像元数据加上该区域各
// 启用节点上的存在状态数组（设计 D7）。
type ImageZoneItem struct {
	Image model.Image       `json:"image"`
	Nodes []NodeImageStatus `json:"nodes"`
}

// scanNodesContent 并行扫描 nodes 的 local/import 存储内容（每节点一次
// ListStorageContent，设计 D1/D7）：单节点失败不影响整体——该节点返回
// nil 清单（表现为"未下载"），错误被吞掉并仅记录日志。同步等待所有节点
// 返回，结果顺序确定（与 nodes 索引对齐；调用方传入的节点均按 id 排序）。
func (s *ImageService) scanNodesContent(ctx context.Context, nodes []model.PVENode) [][]pve.StorageContent {
	files := make([][]pve.StorageContent, len(nodes))
	var wg sync.WaitGroup
	for i := range nodes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			node := nodes[i]
			client := s.newClient(node.Host, node.Port, node.APIUser, node.APITokenSecret)
			items, err := client.ListStorageContent(ctx, nodeName(node), "local", "import")
			if err != nil {
				// 错误被吞掉：该节点降级为"无镜像"，绝不拖垮整个列表请求
				//（节点状态数组允许单节点降级，设计 D7 的容忍策略）。
				slog.Warn("image scan failed on node",
					"node", node.Name, "pve_node", nodeName(node), "error", err)
				return
			}
			files[i] = items
		}(i)
	}
	wg.Wait()
	return files
}

// buildImageStatuses 从预扫描的节点文件清单构建镜像在各节点上的存在状态。
// filesByNode 与 nodes 索引对齐（nil 表示该节点扫描失败，状态降级为未下载）。
// 纯函数：扫描与匹配解耦，使 ListImagesByZone 的"每节点一次扫描 + 多镜像
// 复用"与 GetImageNodeStatus 的"单镜像扫描"共享同一匹配逻辑。
func buildImageStatuses(filesByNode [][]pve.StorageContent, nodes []model.PVENode, image *model.Image) []NodeImageStatus {
	statuses := make([]NodeImageStatus, len(nodes))
	for i := range nodes {
		node := nodes[i]
		st := NodeImageStatus{NodeID: node.ID, NodeName: node.Name, PveName: node.PveName}
		for _, item := range filesByNode[i] {
			if imageFileMatches(image.DownloadURL, item.Name) {
				st.Downloaded = true
				st.VolID = item.VolID
				break
			}
		}
		statuses[i] = st
	}
	return statuses
}

// scanImageOnNodes 扫描单个镜像在 nodes 上的存在状态：并行调用各节点
// ListStorageContent，文件名匹配 download_url 的 basename（设计 D2）。
// 单节点失败不影响整体（该节点 Downloaded=false，错误被吞掉并仅记录日志）。
func (s *ImageService) scanImageOnNodes(ctx context.Context, nodes []model.PVENode, image *model.Image) []NodeImageStatus {
	return buildImageStatuses(s.scanNodesContent(ctx, nodes), nodes, image)
}

// ListImagesByZone 返回一页在区域中存在的镜像。区域必须存在；"存在"指
// 区域内至少一个启用节点上已下载该镜像文件（存在性语义，设计 D3 配套）——
// 与旧版"node_images 交集"语义不同：镜像只要被任一启用节点拥有即认为在
// 区域中可用，创建 VM 时的节点选择会再按节点过滤（见 VMService）。没有
// 启用节点的区域返回空列表而非错误。
//
// 实现：先对该区域所有启用节点做一次 content 扫描（每节点一次，并行），
// 复用预扫描清单组装每个镜像的节点状态并过滤；分页在过滤后应用
// （slicePage，与旧版相同的理由：SQL LIMIT/OFFSET 无法表达过滤后分页，
// 镜像表是小型的元数据集，全量扫描开销很低）。total 是过滤后镜像的数量，
// 与分页无关。
func (s *ImageService) ListImagesByZone(ctx context.Context, zoneID int64, limit, offset int) ([]ImageZoneItem, int, error) {
	exists, err := s.repo.ZoneExists(ctx, zoneID)
	if err != nil {
		return nil, 0, fmt.Errorf("zone existence check: %w", err)
	}
	if !exists {
		return nil, 0, notFoundf("zone %d not found", zoneID)
	}

	nodes, err := s.nodeRepo.ListEnabledNodesByZone(ctx, zoneID)
	if err != nil {
		return nil, 0, fmt.Errorf("enabled nodes by zone: %w", err)
	}

	items := make([]ImageZoneItem, 0)
	if len(nodes) == 0 {
		// 无启用节点时镜像必然不存在于任何节点，无需扫描，直接返回空页。
		return slicePage(items, limit, offset), 0, nil
	}

	images, err := s.repo.List(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list images: %w", err)
	}

	filesByNode := s.scanNodesContent(ctx, nodes)
	for _, img := range images {
		statuses := buildImageStatuses(filesByNode, nodes, &img)
		has := false
		for _, st := range statuses {
			if st.Downloaded {
				has = true
				break
			}
		}
		if has {
			items = append(items, ImageZoneItem{Image: img, Nodes: statuses})
		}
	}
	return slicePage(items, limit, offset), len(items), nil
}

// slicePage 返回 items 的 limit/offset 切片（offset 越界时返回空切片，绝不
// 返回 nil）。供在 Go 中而非 SQL 中分页的列表路径共用。负的 limit/offset 会
// 被钳制为 0——HTTP 层通过 parsePagination 拒绝负值，服务调用方也总是传入
// 非负值，但该包级辅助函数会被 VM/区域/IP 池的测试替身以及仓库调用方复用，
// 因此绝不能因切片运算而 panic，也不能在下游把 LIMIT -1 静默当作"不限制"。
func slicePage[T any](items []T, limit, offset int) []T {
	if limit < 0 {
		limit = 0
	}
	if offset < 0 {
		offset = 0
	}
	start := min(offset, len(items))
	end := min(start+limit, len(items))
	return items[start:end]
}

// Download 受理镜像到一组节点的下载（设计 D6）：目标由 zoneID（区域全部
// 启用节点）或 nodeIDs（显式列表）二选一指定，两者同时提供按 bad_request
// 拒绝（互斥，不做隐式优先）；每节点同步落一条 running 审计记录后启动
// 独立 goroutine 执行下载，终态（success/failed）写回记录。返回本次创建
// 的 running 记录列表，handler 层以 202 返回，前端可轮询 ListImageOperations
// 查看进度。
func (s *ImageService) Download(ctx context.Context, imageID int64, nodeIDs []int64, zoneID *int64) ([]model.ImageOperation, error) {
	if imageID <= 0 {
		return nil, badRequestf("image_id must be > 0")
	}
	if zoneID != nil && len(nodeIDs) > 0 {
		return nil, badRequestf("zone_id and node_ids are mutually exclusive, provide exactly one")
	}
	image, err := s.repo.Get(ctx, imageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("image %d not found", imageID)
		}
		return nil, fmt.Errorf("get image: %w", err)
	}
	// SSRF 面最小化：download_url 必须可解析、为 http(s) 且 host 命中下载源
	// 白名单。下载请求最终由 PVE 节点代发，本服务是唯一可控的校验点——非
	// http(s) 协议（file://、gopher:// 等）或非白名单域名（内网地址、未知
	// 公网源）都会让 PVE 成为任意目标的读取器，受理前拒绝。
	if issue := s.imageDownloadURLIssue(image.DownloadURL); issue != "" {
		return nil, badRequestf("%s", issue)
	}
	// L1：文件名必须是合法的镜像文件名（非空、非 "." / ".."、不含路径
	// 分隔符），防止把目录或上级路径当作下载目标文件名交给 PVE。
	if !validImageFileName(image.DownloadURL) {
		return nil, badRequestf("image download_url must end with a valid image file name")
	}

	// zone 模式（区域目标）先校验区域存在，与 ListImagesByZone/GetImageNodeStatus
	// 的行为一致；nodeIDs 模式不涉及区域，无需该校验。
	if zoneID != nil {
		exists, err := s.repo.ZoneExists(ctx, *zoneID)
		if err != nil {
			return nil, fmt.Errorf("zone existence check: %w", err)
		}
		if !exists {
			return nil, notFoundf("zone %d not found", *zoneID)
		}
	}

	nodes, err := s.resolveDownloadNodes(ctx, nodeIDs, zoneID)
	if err != nil {
		return nil, err
	}

	// 幂等：目标节点上存在该镜像未终态（running）的下载操作时拒绝受理，
	// 避免对同一节点重复下载。全部节点检查通过后才开始落 running 记录，
	// 保证"要么全部受理，要么全部拒绝"的语义。HasRunningOperation 检查与
	// CreateOperation 落库非原子，并发请求存在双双通过检查的窗口；契约已
	// 声明并发竞态下可能产生重复记录，由调用方去重。
	for _, node := range nodes {
		running, err := s.opRepo.HasRunningOperation(ctx, image.ID, node.ID)
		if err != nil {
			return nil, fmt.Errorf("check running download on node %d: %w", node.ID, err)
		}
		if running {
			// 状态冲突语义（409 conflict）：镜像在节点上已有未终态下载。
			return nil, conflictf("image %d is already being downloaded on node %d", image.ID, node.ID)
		}
	}

	operations := make([]model.ImageOperation, 0, len(nodes))
	for _, node := range nodes {
		op, err := s.opRepo.CreateOperation(ctx, model.ImageOperation{
			ImageID: image.ID,
			NodeID:  node.ID,
			Action:  model.ImageOpActionDownload,
			Result:  model.ImageOpResultRunning,
		})
		if err != nil {
			// 先落 running 再启动 goroutine：中途落库失败返回错误，但此前
			// 已启动的下载继续执行（写回各自的终态），属异步编排的既定
			// 语义——调用方看到错误时可重试，重复受理由运维/前端判断。
			return nil, fmt.Errorf("create download operation on node %d: %w", node.ID, err)
		}
		operations = append(operations, *op)
		// 每节点一个独立 goroutine：互不阻塞，单节点失败不影响其他节点。
		go s.downloadToNode(image, node, op.ID)
	}
	return operations, nil
}

// resolveDownloadNodes 解析下载目标节点集合：zoneID 非空时取区域全部启用
// 节点，否则按 nodeIDs 逐个解析（不存在的节点返回 not_found 并指明 id；
// 重复的 id 按出现顺序去重，保留首次出现的，避免对同一节点并发下载）。
// nodeIDs 数量超过 maxImageDownloadNodes 时返回 bad_request（批量上限）。
// 空集合返回 bad_request（没有可下载的节点）。
func (s *ImageService) resolveDownloadNodes(ctx context.Context, nodeIDs []int64, zoneID *int64) ([]model.PVENode, error) {
	var nodes []model.PVENode
	var err error
	if zoneID != nil {
		nodes, err = s.nodeRepo.ListEnabledNodesByZone(ctx, *zoneID)
		if err != nil {
			return nil, fmt.Errorf("list enabled nodes by zone: %w", err)
		}
	} else {
		if len(nodeIDs) > maxImageDownloadNodes {
			return nil, badRequestf("node_ids exceeds the limit of %d nodes per download request", maxImageDownloadNodes)
		}
		seen := make(map[int64]struct{}, len(nodeIDs))
		for _, id := range nodeIDs {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			node, err := s.nodeRepo.GetNode(ctx, id)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, notFoundf("node %d not found", id)
				}
				return nil, fmt.Errorf("get node %d: %w", id, err)
			}
			nodes = append(nodes, *node)
		}
	}
	if len(nodes) == 0 {
		return nil, badRequestf("no target nodes for image download")
	}
	return nodes, nil
}

// downloadToNode 在独立 goroutine 中执行单节点镜像下载编排（设计 D6）：
// DownloadURL 受理（得 UPID，受理失败直接落 failed、upid 保持空串）→
// WaitTask 轮询至终态 → 结果写回 image_operations。goroutine 使用
// background context + imageDownloadTimeout（不随请求 ctx 取消，与
// provisionVM 同模式）；任何 panic 都会被恢复并记录为 failed，绝不能
// 拖垮进程。
func (s *ImageService) downloadToNode(image *model.Image, node model.PVENode, opID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), imageDownloadTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			// panic 原因先脱敏（含 download_url 原文替换，URL 可能携带签名
			// 等敏感参数）再带上统一前缀，使运维能区分崩溃与常规失败；该
			// 消息已脱敏，直接落库（不再走 finishDownload 的二次脱敏，否则
			// 前缀会被 last-colon 截取剥掉）。
			msg := truncateImageOpError("internal panic during image download: " +
				sanitizeImageOperationErrorMessage(fmt.Errorf("%v", r), image.DownloadURL))
			if uerr := s.opRepo.UpdateOperationResult(ctx, opID, model.ImageOpResultFailed, msg, ""); uerr != nil {
				slog.Error("could not persist image download failure", "op_id", opID, "error", uerr)
			}
			slog.Error("image download panicked",
				"op_id", opID, "image_id", image.ID, "node", node.Name, "pve_node", nodeName(node))
		}
	}()

	client := s.newClient(node.Host, node.Port, node.APIUser, node.APITokenSecret)
	upid, err := client.DownloadURL(ctx, nodeName(node), "local", "import", imageFileName(image.DownloadURL), image.DownloadURL)
	if err != nil {
		// PVE 拒绝受理（网络失败/参数错误）：记录 failed，upid 保持空串。
		s.finishDownload(ctx, opID, model.ImageOpResultFailed, err, "", image.DownloadURL)
		return
	}
	if _, err := client.WaitTask(ctx, nodeName(node), upid, 0, 0); err != nil {
		s.finishDownload(ctx, opID, model.ImageOpResultFailed, err, upid, image.DownloadURL)
		return
	}
	s.finishDownload(ctx, opID, model.ImageOpResultSuccess, nil, upid, image.DownloadURL)
}

// finishDownload 把单节点下载的终态（success/failed）写回 image_operations。
// 失败消息经脱敏（含 download_url 原文替换，URL 可能携带签名等敏感参数）
// 与截断后落库（对外不暴露 PVE 内部细节，红线）；落库失败只记录日志
//（goroutine 内无法再向外传播）。
func (s *ImageService) finishDownload(ctx context.Context, opID int64, result string, err error, upid, downloadURL string) {
	msg := ""
	if err != nil {
		msg = sanitizeImageOperationError(err, downloadURL)
	}
	if uerr := s.opRepo.UpdateOperationResult(ctx, opID, result, msg, upid); uerr != nil {
		slog.Error("could not persist image download result", "op_id", opID, "error", uerr)
	}
}

// sanitizeImageOperationError 生成失败下载记录（image_operations.error_message）
// 的落库值：把错误链中的 PVE 部分替换为脱敏摘要、把错误消息中出现的
// download_url 原文替换为 [redacted]（URL 可能携带签名等敏感查询参数，
// 绝不落库），并按 maxImageOpErrorLen 截断。先脱敏后截断（与 VM 服务的
// sanitizeOperationError 相同顺序），按 rune 边界切割，多字节 UTF-8 字符
// 绝不会被切成非法序列（VARCHAR 列会拒绝它们，Postgres 22001）。
func sanitizeImageOperationError(err error, downloadURL string) string {
	return truncateImageOpError(sanitizeImageOperationErrorMessage(err, downloadURL))
}

// sanitizeImageOperationErrorMessage 把错误转换为对外可展示的脱敏摘要并
// 替换其中出现的 download_url 原文（不做长度截断，供 finishDownload 与
// panic 恢复路径共用）。PVE 上游错误走 sanitizePVEError 的 errors 摘要
//（消息原样保留，不截取），直接对摘要替换 URL；普通错误先替换 URL 再走
// sanitizePVEError——否则其 last-colon 截取会剥掉 URL 的 scheme 前缀，
// 使整体替换失配、泄漏完整 URL。
func sanitizeImageOperationErrorMessage(err error, downloadURL string) string {
	var upErr *pve.UpstreamError
	if errors.As(err, &upErr) {
		return redactImageDownloadURL(sanitizePVEError(err), downloadURL)
	}
	return sanitizePVEError(errors.New(redactImageDownloadURL(err.Error(), downloadURL)))
}

// redactImageDownloadURL 将消息中出现的 download_url 原文整体替换为
// [redacted]（download_url 可能携带签名等敏感查询参数，绝不能出现在
// 落库的 error_message 中）。
func redactImageDownloadURL(msg, downloadURL string) string {
	if downloadURL == "" {
		return msg
	}
	return strings.ReplaceAll(msg, downloadURL, "[redacted]")
}

// truncateImageOpError 按 maxImageOpErrorLen 以 rune 边界截断错误消息。
func truncateImageOpError(msg string) string {
	r := []rune(msg)
	if len(r) > maxImageOpErrorLen {
		return string(r[:maxImageOpErrorLen])
	}
	return msg
}

// GetImageNodeStatus 返回单个镜像在各启用节点上的存在状态（设计 D7）：
// zoneID 非空时只扫该区域启用节点（区域必须存在，否则 not_found），否则
// 扫全部启用节点。镜像必须存在。返回的状态数组按节点 id 排序（节点列表
// 按 id 排序，状态与节点索引对齐）。
func (s *ImageService) GetImageNodeStatus(ctx context.Context, imageID int64, zoneID *int64) ([]NodeImageStatus, error) {
	image, err := s.repo.Get(ctx, imageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("image %d not found", imageID)
		}
		return nil, fmt.Errorf("get image: %w", err)
	}
	if zoneID != nil {
		exists, err := s.repo.ZoneExists(ctx, *zoneID)
		if err != nil {
			return nil, fmt.Errorf("zone existence check: %w", err)
		}
		if !exists {
			return nil, notFoundf("zone %d not found", *zoneID)
		}
	}
	var nodes []model.PVENode
	if zoneID != nil {
		nodes, err = s.nodeRepo.ListEnabledNodesByZone(ctx, *zoneID)
	} else {
		nodes, err = s.nodeRepo.ListAllEnabledNodes(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("list enabled nodes: %w", err)
	}
	return s.scanImageOnNodes(ctx, nodes, image), nil
}

// ListImageOperations 返回指定镜像的一页下载操作记录（按创建时间倒序）及
// 匹配总数，供下载进度与审计查询。镜像必须存在。
func (s *ImageService) ListImageOperations(ctx context.Context, imageID int64, limit, offset int) ([]model.ImageOperation, int, error) {
	if _, err := s.repo.Get(ctx, imageID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, notFoundf("image %d not found", imageID)
		}
		return nil, 0, fmt.Errorf("get image: %w", err)
	}
	ops, total, err := s.opRepo.ListOperationsByImage(ctx, imageID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list image operations: %w", err)
	}
	return ops, total, nil
}
