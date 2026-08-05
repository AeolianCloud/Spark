package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark/model"
)

// ImageRepository persists model.Image rows, including the node_images JSONB
// map (node name -> storage path or presence marker).
type ImageRepository struct {
	pool *pgxpool.Pool
}

// NewImageRepository creates an ImageRepository backed by pool.
func NewImageRepository(pool *pgxpool.Pool) *ImageRepository {
	return &ImageRepository{pool: pool}
}

const imageCols = "id, name, default_user, node_images, created_at"

// Create inserts an image and returns it with id and created_at filled. A nil
// nodeImages is normalized to an empty map so the JSONB column is written as
// '{}' (migration convention), never as SQL NULL. A duplicate name yields
// ErrConflict.
func (r *ImageRepository) Create(ctx context.Context, name, defaultUser string, nodeImages map[string]string) (*model.Image, error) {
	if nodeImages == nil {
		nodeImages = map[string]string{}
	}
	img := &model.Image{Name: name, DefaultUser: defaultUser, NodeImages: nodeImages}
	err := r.pool.QueryRow(ctx,
		"INSERT INTO images (name, default_user, node_images) VALUES ($1, $2, $3) RETURNING id, created_at",
		name, defaultUser, nodeImages,
	).Scan(&img.ID, &img.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return img, nil
}

// Get returns the image with the given id, or pgx.ErrNoRows when absent.
func (r *ImageRepository) Get(ctx context.Context, id int64) (*model.Image, error) {
	var img model.Image
	err := r.pool.QueryRow(ctx, "SELECT "+imageCols+" FROM images WHERE id=$1", id).
		Scan(&img.ID, &img.Name, &img.DefaultUser, &img.NodeImages, &img.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &img, nil
}

// GetByName returns the image with the given name, or pgx.ErrNoRows when
// absent.
func (r *ImageRepository) GetByName(ctx context.Context, name string) (*model.Image, error) {
	var img model.Image
	err := r.pool.QueryRow(ctx, "SELECT "+imageCols+" FROM images WHERE name=$1", name).
		Scan(&img.ID, &img.Name, &img.DefaultUser, &img.NodeImages, &img.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &img, nil
}

// List returns all images ordered by id.
func (r *ImageRepository) List(ctx context.Context) ([]model.Image, error) {
	rows, err := r.pool.Query(ctx, "SELECT "+imageCols+" FROM images ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("images: list: %w", err)
	}
	defer rows.Close()

	images := make([]model.Image, 0)
	for rows.Next() {
		var img model.Image
		if err := rows.Scan(&img.ID, &img.Name, &img.DefaultUser, &img.NodeImages, &img.CreatedAt); err != nil {
			return nil, fmt.Errorf("images: scan: %w", err)
		}
		images = append(images, img)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("images: iterate: %w", err)
	}
	return images, nil
}

// ZoneExists reports whether a zone with the given id exists. It backs
// ImageService.ListImagesByZone.
func (r *ImageRepository) ZoneExists(ctx context.Context, id int64) (bool, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM zones WHERE id=$1)", id).Scan(&exists); err != nil {
		return false, fmt.Errorf("images: zone exists: %w", err)
	}
	return exists, nil
}

// EnabledNodeNamesByZone returns the names of the enabled nodes in a zone,
// ordered by id. It backs ImageService.ListImagesByZone.
func (r *ImageRepository) EnabledNodeNamesByZone(ctx context.Context, zoneID int64) ([]string, error) {
	rows, err := r.pool.Query(ctx, "SELECT name FROM pve_nodes WHERE zone_id=$1 AND enabled=TRUE ORDER BY id", zoneID)
	if err != nil {
		return nil, fmt.Errorf("images: enabled nodes by zone: %w", err)
	}
	defer rows.Close()

	names := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("images: scan node name: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("images: iterate node names: %w", err)
	}
	return names, nil
}
