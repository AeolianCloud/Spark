package repository

import (
	"context"
	"fmt"

	"spark/model"
)

// ZoneRepository persists model.Zone rows.
type ZoneRepository struct {
	pool pgxQuerier
}

// NewZoneRepository creates a ZoneRepository backed by pool.
func NewZoneRepository(pool pgxQuerier) *ZoneRepository {
	return &ZoneRepository{pool: pool}
}

const zoneCols = "id, name, created_at"

// CreateZone inserts a zone and returns it with id and created_at filled.
func (r *ZoneRepository) CreateZone(ctx context.Context, name string) (*model.Zone, error) {
	z := &model.Zone{Name: name}
	err := r.pool.QueryRow(ctx,
		"INSERT INTO zones (name) VALUES ($1) RETURNING id, created_at", name,
	).Scan(&z.ID, &z.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return z, nil
}

// GetZone returns the zone with the given id, or pgx.ErrNoRows when absent.
func (r *ZoneRepository) GetZone(ctx context.Context, id int64) (*model.Zone, error) {
	var z model.Zone
	err := r.pool.QueryRow(ctx, "SELECT "+zoneCols+" FROM zones WHERE id=$1", id).
		Scan(&z.ID, &z.Name, &z.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &z, nil
}

// ListZones returns all zones ordered by id.
func (r *ZoneRepository) ListZones(ctx context.Context) ([]model.Zone, error) {
	rows, err := r.pool.Query(ctx, "SELECT "+zoneCols+" FROM zones ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("zones: list: %w", err)
	}
	defer rows.Close()

	zones := make([]model.Zone, 0)
	for rows.Next() {
		var z model.Zone
		if err := rows.Scan(&z.ID, &z.Name, &z.CreatedAt); err != nil {
			return nil, fmt.Errorf("zones: scan: %w", err)
		}
		zones = append(zones, z)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("zones: iterate: %w", err)
	}
	return zones, nil
}
