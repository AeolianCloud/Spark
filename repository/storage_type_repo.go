package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark/model"
)

// StorageTypeRepository persists model.StorageType rows.
type StorageTypeRepository struct {
	pool *pgxpool.Pool
}

// NewStorageTypeRepository creates a StorageTypeRepository backed by pool.
func NewStorageTypeRepository(pool *pgxpool.Pool) *StorageTypeRepository {
	return &StorageTypeRepository{pool: pool}
}

const storageTypeCols = "id, name, display_name, pve_storage, created_at"

// Create inserts a storage type and returns it with id and created_at filled.
// A duplicate name yields ErrConflict.
func (r *StorageTypeRepository) Create(ctx context.Context, name, displayName, pveStorage string) (*model.StorageType, error) {
	st := &model.StorageType{Name: name, DisplayName: displayName, PVEStorage: pveStorage}
	err := r.pool.QueryRow(ctx,
		"INSERT INTO storage_types (name, display_name, pve_storage) VALUES ($1, $2, $3) RETURNING id, created_at",
		name, displayName, pveStorage,
	).Scan(&st.ID, &st.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return st, nil
}

// List returns all storage types ordered by id.
func (r *StorageTypeRepository) List(ctx context.Context) ([]model.StorageType, error) {
	rows, err := r.pool.Query(ctx, "SELECT "+storageTypeCols+" FROM storage_types ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("storage types: list: %w", err)
	}
	defer rows.Close()

	types := make([]model.StorageType, 0)
	for rows.Next() {
		var st model.StorageType
		if err := rows.Scan(&st.ID, &st.Name, &st.DisplayName, &st.PVEStorage, &st.CreatedAt); err != nil {
			return nil, fmt.Errorf("storage types: scan: %w", err)
		}
		types = append(types, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage types: iterate: %w", err)
	}
	return types, nil
}

// ListPage returns one page of storage types ordered by id. It feeds the
// paginated GET /storage-types endpoint.
func (r *StorageTypeRepository) ListPage(ctx context.Context, limit, offset int) ([]model.StorageType, error) {
	rows, err := r.pool.Query(ctx,
		"SELECT "+storageTypeCols+" FROM storage_types ORDER BY id LIMIT $1 OFFSET $2", limit, offset)
	if err != nil {
		return nil, fmt.Errorf("storage types: list page: %w", err)
	}
	defer rows.Close()

	types := make([]model.StorageType, 0)
	for rows.Next() {
		var st model.StorageType
		if err := rows.Scan(&st.ID, &st.Name, &st.DisplayName, &st.PVEStorage, &st.CreatedAt); err != nil {
			return nil, fmt.Errorf("storage types: scan: %w", err)
		}
		types = append(types, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage types: iterate: %w", err)
	}
	return types, nil
}

// Count returns the total number of storage types, backing the
// X-Total-Count header of GET /storage-types.
func (r *StorageTypeRepository) Count(ctx context.Context) (int, error) {
	var n int
	if err := r.pool.QueryRow(ctx, "SELECT count(*) FROM storage_types").Scan(&n); err != nil {
		return 0, fmt.Errorf("storage types: count: %w", err)
	}
	return n, nil
}

// Get returns the storage type with the given id, or pgx.ErrNoRows when
// absent.
func (r *StorageTypeRepository) Get(ctx context.Context, id int64) (*model.StorageType, error) {
	var st model.StorageType
	err := r.pool.QueryRow(ctx, "SELECT "+storageTypeCols+" FROM storage_types WHERE id=$1", id).
		Scan(&st.ID, &st.Name, &st.DisplayName, &st.PVEStorage, &st.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &st, nil
}

// Update replaces the metadata of the storage type with the given id and
// returns the updated row. It returns pgx.ErrNoRows when absent and
// ErrConflict on a duplicate name. Existing VMs keep referencing the row by
// id, so updating the mapping never rewrites them (pure metadata).
func (r *StorageTypeRepository) Update(ctx context.Context, id int64, name, displayName, pveStorage string) (*model.StorageType, error) {
	var st model.StorageType
	err := r.pool.QueryRow(ctx,
		"UPDATE storage_types SET name=$1, display_name=$2, pve_storage=$3 WHERE id=$4 RETURNING "+storageTypeCols,
		name, displayName, pveStorage, id,
	).Scan(&st.ID, &st.Name, &st.DisplayName, &st.PVEStorage, &st.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &st, nil
}

// Delete removes the storage type with the given id. In the same transaction
// it refuses the delete when any VM still references the row (ErrInUse), so
// existing VMs keep their storage mapping. It returns pgx.ErrNoRows when the
// id does not exist.
func (r *StorageTypeRepository) Delete(ctx context.Context, id int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("storage types: begin delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var inUse bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM vms WHERE storage_type_id=$1)", id).Scan(&inUse); err != nil {
		return fmt.Errorf("storage types: check vms references: %w", err)
	}
	if inUse {
		return ErrInUse
	}

	tag, err := tx.Exec(ctx, "DELETE FROM storage_types WHERE id=$1", id)
	if err != nil {
		return classifyDBError(err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("storage types: commit delete: %w", err)
	}
	return nil
}

// classifyDBError maps well-known database errors onto repository sentinels.
// Everything else (including pgx.ErrNoRows) is returned unchanged so callers
// can match on it directly. Shared by every repository (storage types,
// images, zones, nodes, IP pools) via the package-level helper.
func classifyDBError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return ErrConflict
		case "23503": // foreign_key_violation
			return ErrInUse
		}
	}
	return err
}
