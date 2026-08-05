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

// StorageTypeService 实现存储类型的业务规则；存储类型是抽象 PVE 存储的
// 元数据（name/display_name/pve_storage）。
type StorageTypeService struct {
	repo *repository.StorageTypeRepository
}

// NewStorageTypeService 使用 repo 创建一个 StorageTypeService。
func NewStorageTypeService(repo *repository.StorageTypeRepository) *StorageTypeService {
	return &StorageTypeService{repo: repo}
}

// Create 校验字段并持久化一个新的存储类型。名称重复视为冲突。
func (s *StorageTypeService) Create(ctx context.Context, name, displayName, pveStorage string) (*model.StorageType, error) {
	if err := validateStorageType(name, displayName, pveStorage); err != nil {
		return nil, err
	}
	st, err := s.repo.Create(ctx, strings.TrimSpace(name), strings.TrimSpace(displayName), strings.TrimSpace(pveStorage))
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, conflictf("storage type name %q already exists", name)
		}
		return nil, fmt.Errorf("create storage type: %w", err)
	}
	return st, nil
}

// List 返回按 id 排序的一页存储类型；total 是存储类型总数，与分页无关。
func (s *StorageTypeService) List(ctx context.Context, limit, offset int) ([]model.StorageType, int, error) {
	types, err := s.repo.ListPage(ctx, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list storage types: %w", err)
	}
	total, err := s.repo.Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list storage types: count: %w", err)
	}
	return types, total, nil
}

// Get 返回指定 id 的存储类型，或返回 not_found 错误。
func (s *StorageTypeService) Get(ctx context.Context, id int64) (*model.StorageType, error) {
	st, err := s.repo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("storage type %d not found", id)
		}
		return nil, fmt.Errorf("get storage type: %w", err)
	}
	return st, nil
}

// Update 替换已有存储类型的元数据。更新映射不会改写已有 VM：storage_types
// 是纯元数据，VM 按 id 引用该行，因此该变更只影响后续的供给。
func (s *StorageTypeService) Update(ctx context.Context, id int64, name, displayName, pveStorage string) (*model.StorageType, error) {
	if err := validateStorageType(name, displayName, pveStorage); err != nil {
		return nil, err
	}
	st, err := s.repo.Update(ctx, id, strings.TrimSpace(name), strings.TrimSpace(displayName), strings.TrimSpace(pveStorage))
	if err != nil {
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil, notFoundf("storage type %d not found", id)
		case errors.Is(err, repository.ErrConflict):
			return nil, conflictf("storage type name %q already exists", name)
		}
		return nil, fmt.Errorf("update storage type: %w", err)
	}
	return st, nil
}

// Delete 删除一个存储类型。当仍有 VM 引用它时，删除会被以冲突错误拒绝，
// 从而保证现有 VM 的映射保持不变。
func (s *StorageTypeService) Delete(ctx context.Context, id int64) error {
	err := s.repo.Delete(ctx, id)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return notFoundf("storage type %d not found", id)
	case errors.Is(err, repository.ErrInUse):
		return conflictf("storage type %d is still referenced by VMs", id)
	default:
		return fmt.Errorf("delete storage type: %w", err)
	}
}

// validateStorageType 强制要求每个字段均非空。
func validateStorageType(name, displayName, pveStorage string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return badRequestf("storage type name is required")
	case strings.TrimSpace(displayName) == "":
		return badRequestf("storage type display_name is required")
	case strings.TrimSpace(pveStorage) == "":
		return badRequestf("storage type pve_storage is required")
	}
	return nil
}
