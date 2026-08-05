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

// StorageTypeService implements the business rules for storage types, the
// metadata abstracting a PVE storage (name/display_name/pve_storage).
type StorageTypeService struct {
	repo *repository.StorageTypeRepository
}

// NewStorageTypeService creates a StorageTypeService backed by repo.
func NewStorageTypeService(repo *repository.StorageTypeRepository) *StorageTypeService {
	return &StorageTypeService{repo: repo}
}

// Create validates the fields and persists a new storage type. A duplicate
// name is a conflict.
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

// List returns all storage types.
func (s *StorageTypeService) List(ctx context.Context) ([]model.StorageType, error) {
	types, err := s.repo.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list storage types: %w", err)
	}
	return types, nil
}

// Get returns the storage type with the given id, or a not_found error.
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

// Update replaces the metadata of an existing storage type. Updating the
// mapping never rewrites existing VMs: storage_types is pure metadata and VMs
// reference the row by id, so the change only affects future provisioning.
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

// Delete removes a storage type. Deletion is refused with a conflict while
// any VM still references it, so existing VMs keep their mapping intact.
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

// validateStorageType enforces that every field is non-empty.
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
