package repository

import (
	"context"
	"fmt"

	"spark/model"
)

// ImageRepository 负责持久化 model.Image 行。镜像在各节点上的存在状态
// 以 PVE 实时扫描为准，不落库；本仓库仅管理名称、默认登录用户与下载
// 地址（download_url）等元数据。
type ImageRepository struct {
	pool pgxQuerier
}

// NewImageRepository 创建由 pool 支撑的 ImageRepository。
func NewImageRepository(pool pgxQuerier) *ImageRepository {
	return &ImageRepository{pool: pool}
}

// imageCols 是 images 的读取列清单。
const imageCols = "id, name, default_user, download_url, created_at"

// Create 插入一个镜像并返回它，且已填充 id 与 created_at。调用方
// （service 层）保证 name/default_user/download_url 均非空——repository
// 不做业务校验（download_url 非空校验在 service 层，与 name 一致）。
// 名称重复时产生 ErrConflict。
func (r *ImageRepository) Create(ctx context.Context, name, defaultUser, downloadURL string) (*model.Image, error) {
	img := &model.Image{Name: name, DefaultUser: defaultUser, DownloadURL: downloadURL}
	err := r.pool.QueryRow(ctx,
		"INSERT INTO images (name, default_user, download_url) VALUES ($1, $2, $3) RETURNING id, created_at",
		name, defaultUser, downloadURL,
	).Scan(&img.ID, &img.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return img, nil
}

// Get 返回指定 id 的镜像；不存在时返回 pgx.ErrNoRows。
func (r *ImageRepository) Get(ctx context.Context, id int64) (*model.Image, error) {
	var img model.Image
	err := r.pool.QueryRow(ctx, "SELECT "+imageCols+" FROM images WHERE id=$1", id).
		Scan(&img.ID, &img.Name, &img.DefaultUser, &img.DownloadURL, &img.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &img, nil
}

// GetByName 返回指定名称的镜像；不存在时返回 pgx.ErrNoRows。
func (r *ImageRepository) GetByName(ctx context.Context, name string) (*model.Image, error) {
	var img model.Image
	err := r.pool.QueryRow(ctx, "SELECT "+imageCols+" FROM images WHERE name=$1", name).
		Scan(&img.ID, &img.Name, &img.DefaultUser, &img.DownloadURL, &img.CreatedAt)
	if err != nil {
		return nil, classifyDBError(err)
	}
	return &img, nil
}

// List 返回按 id 排序的全部镜像。
func (r *ImageRepository) List(ctx context.Context) ([]model.Image, error) {
	rows, err := r.pool.Query(ctx, "SELECT "+imageCols+" FROM images ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("images: list: %w", err)
	}
	defer rows.Close()

	images := make([]model.Image, 0)
	for rows.Next() {
		var img model.Image
		if err := rows.Scan(&img.ID, &img.Name, &img.DefaultUser, &img.DownloadURL, &img.CreatedAt); err != nil {
			return nil, fmt.Errorf("images: scan: %w", err)
		}
		images = append(images, img)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("images: iterate: %w", err)
	}
	return images, nil
}

// ListPage 返回按 id 排序的一页镜像。它服务于分页的 GET /images
// 端点（不带区域过滤）。
func (r *ImageRepository) ListPage(ctx context.Context, limit, offset int) ([]model.Image, error) {
	rows, err := r.pool.Query(ctx,
		"SELECT "+imageCols+" FROM images ORDER BY id LIMIT $1 OFFSET $2", limit, offset)
	if err != nil {
		return nil, fmt.Errorf("images: list page: %w", err)
	}
	defer rows.Close()

	images := make([]model.Image, 0)
	for rows.Next() {
		var img model.Image
		if err := rows.Scan(&img.ID, &img.Name, &img.DefaultUser, &img.DownloadURL, &img.CreatedAt); err != nil {
			return nil, fmt.Errorf("images: scan: %w", err)
		}
		images = append(images, img)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("images: iterate: %w", err)
	}
	return images, nil
}

// Count 返回镜像总数，支撑 GET /images 的 X-Total-Count 响应头。
func (r *ImageRepository) Count(ctx context.Context) (int, error) {
	var n int
	if err := r.pool.QueryRow(ctx, "SELECT count(*) FROM images").Scan(&n); err != nil {
		return 0, fmt.Errorf("images: count: %w", err)
	}
	return n, nil
}

// ZoneExists 报告指定 id 的区域是否存在。它支撑
// ImageService.ListImagesByZone。
func (r *ImageRepository) ZoneExists(ctx context.Context, id int64) (bool, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM zones WHERE id=$1)", id).Scan(&exists); err != nil {
		return false, fmt.Errorf("images: zone exists: %w", err)
	}
	return exists, nil
}
