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

// ImageService implements the business rules for registered cloud images.
type ImageService struct {
	repo *repository.ImageRepository
}

// NewImageService creates an ImageService backed by repo.
func NewImageService(repo *repository.ImageRepository) *ImageService {
	return &ImageService{repo: repo}
}

// Create validates the fields and persists a new image. A duplicate name is a
// conflict. A nil nodeImages is normalized to an empty map by the repository
// so the JSONB column is written as '{}'. The node_images keys are trimmed
// before validation and persistence, so whitespace-padded node names cannot
// create entries that never match the enabled-node list of a zone.
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

// Get returns the image with the given id, or a not_found error.
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

// GetByName returns the image with the given name, or a not_found error.
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

// List returns one page of all registered images with their full node_images
// map; total is the total number of images, independent of the page.
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

// ListImagesByZone returns one page of the images available in a zone. The
// zone must exist; an image is available only when its node_images map
// contains every enabled node of the zone (intersection semantics). A zone
// without enabled nodes yields an empty list, not an error.
//
// Pagination is applied AFTER the availability filter: the intersection is a
// service-level rule that SQL LIMIT/OFFSET cannot express (it would slice
// before filtering and produce short or empty pages), so the full registered
// list is scanned and the available slice is paged in Go. The images table
// is a small metadata set, so the full scan is cheap. total is the number of
// available images, independent of the page.
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

// slicePage returns the limit/offset slice of items (offset past the end
// yields an empty slice, never nil). Shared by the paginated list paths that
// page in Go instead of SQL. Negative limit/offset are clamped to 0 — the
// HTTP layer rejects negative values via parsePagination and the service
// callers always pass non-negative values, but this package-level helper is
// reused by VM/zone/IP-pool test doubles and repo callers must not panic on
// the slice arithmetic or silently turn LIMIT -1 into "no limit" downstream.
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

// filterImagesAvailableByNodes keeps the images whose node_images map
// contains a key for every node in nodes. Presence on a node is decided by
// key membership (the value holds the storage path or a presence marker). An
// empty node list yields an empty result, never nil, so JSON serializes as
// [].
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
