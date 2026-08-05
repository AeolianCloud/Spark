package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"spark/model"
	"spark/repository"
)

// ImageService 实现已注册云镜像的业务规则。
type ImageService struct {
	repo *repository.ImageRepository
}

// NewImageService 使用 repo 创建一个 ImageService。
func NewImageService(repo *repository.ImageRepository) *ImageService {
	return &ImageService{repo: repo}
}

// Create 校验字段并持久化一个新的镜像。名称重复视为冲突。nil 的 nodeImages
// 会被仓库规范化为空 map，因此 JSONB 列以 '{}' 写入。node_images 的键在校验
// 和持久化前会被去掉首尾空白，这样带空白填充的节点名不会产生永远无法匹配
// 区域启用节点列表的条目。
func (s *ImageService) Create(ctx context.Context, name, defaultUser string, nodeImages map[string]string) (*model.Image, error) {
	switch {
	case strings.TrimSpace(name) == "":
		return nil, badRequestf("image name is required")
	case strings.TrimSpace(defaultUser) == "":
		return nil, badRequestf("image default_user is required")
	}
	trimmed := make(map[string]string, len(nodeImages))
	for node, path := range nodeImages {
		node = strings.TrimSpace(node)
		if node == "" {
			return nil, badRequestf("image node_images keys must not be empty")
		}
		trimmed[node] = path
	}
	img, err := s.repo.Create(ctx, strings.TrimSpace(name), strings.TrimSpace(defaultUser), trimmed)
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

// List 返回一页全部已注册镜像及其完整 node_images 映射；total 是镜像总数，
// 与分页无关。
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

// ListImagesByZone 返回一页在区域中可用的镜像。区域必须存在；只有当镜像的
// node_images 映射包含区域的每个启用节点时，该镜像才可用（交集语义）。没有
// 启用节点的区域返回空列表而非错误。
//
// 分页在可用性过滤之后应用：交集是 SQL LIMIT/OFFSET 无法表达的服务层规则
// （它会在过滤前切片并产生过短或空的页），因此会扫描全部已注册列表，并在
// Go 中对可用切片分页。images 表是小型的元数据集，全量扫描开销很低。total
// 是可用镜像的数量，与分页无关。
func (s *ImageService) ListImagesByZone(ctx context.Context, zoneID int64, limit, offset int) ([]model.Image, int, error) {
	exists, err := s.repo.ZoneExists(ctx, zoneID)
	if err != nil {
		return nil, 0, fmt.Errorf("zone existence check: %w", err)
	}
	if !exists {
		return nil, 0, notFoundf("zone %d not found", zoneID)
	}

	nodes, err := s.repo.EnabledNodeNamesByZone(ctx, zoneID)
	if err != nil {
		return nil, 0, fmt.Errorf("enabled nodes by zone: %w", err)
	}

	images, err := s.repo.List(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list images: %w", err)
	}
	available := filterImagesAvailableByNodes(images, nodes)
	return slicePage(available, limit, offset), len(available), nil
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

// filterImagesAvailableByNodes 保留 node_images 映射中包含 nodes 中每个节点
// 键的镜像。节点上是否存在由键的成员关系决定（值存放存储路径或存在性标记）。
// 空节点列表产生空结果而非 nil，因此 JSON 序列化为 []。
func filterImagesAvailableByNodes(images []model.Image, nodes []string) []model.Image {
	available := make([]model.Image, 0, len(images))
	if len(nodes) == 0 {
		return available
	}

	nodeSet := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		nodeSet[node] = struct{}{}
	}

	for _, img := range images {
		hasAll := true
		for node := range nodeSet {
			if _, ok := img.NodeImages[node]; !ok {
				hasAll = false
				break
			}
		}
		if hasAll {
			available = append(available, img)
		}
	}
	return available
}
