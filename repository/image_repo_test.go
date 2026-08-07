package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

// imageRowColumns 是 imageCols 扫描顺序对应的列名（5 列，含 download_url），
// 供 mock 行构造使用。
var imageRowColumns = []string{"id", "name", "default_user", "download_url", "created_at"}

// createImageSQL 是 Create 运行的确切的 INSERT ... RETURNING 语句。
const createImageSQL = "INSERT INTO images (name, default_user, download_url) VALUES ($1, $2, $3) RETURNING id, created_at"

// TestImageCreate 验证镜像插入：download_url 一并落库，返回带
// id/created_at 的行。
func TestImageCreate(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery(createImageSQL).
		WithArgs("ubuntu-24.04", "ubuntu", "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img").
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).AddRow(int64(7), testTime))

	repo := NewImageRepository(mock)
	img, err := repo.Create(context.Background(), "ubuntu-24.04", "ubuntu",
		"https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if img.ID != 7 || img.Name != "ubuntu-24.04" || img.DefaultUser != "ubuntu" ||
		img.DownloadURL != "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img" {
		t.Fatalf("img = %+v, want id 7 / name / default_user / download_url", img)
	}
	if !img.CreatedAt.Equal(testTime) {
		t.Fatalf("created_at = %v, want %v", img.CreatedAt, testTime)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestImageCreateDuplicateConflict 钉死重名镜像的 23505 唯一约束冲突
// 映射为 ErrConflict，服务层据此返回 409。
func TestImageCreateDuplicateConflict(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery(createImageSQL).
		WithArgs("ubuntu-24.04", "ubuntu", "https://example.com/ubuntu.img").
		WillReturnError(&pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint \"images_name_key\""})

	repo := NewImageRepository(mock)
	_, err := repo.Create(context.Background(), "ubuntu-24.04", "ubuntu", "https://example.com/ubuntu.img")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestImageGet 验证按 id 读取：download_url 列扫描进 DownloadURL。
func TestImageGet(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT " + imageCols + " FROM images WHERE id=$1").
		WithArgs(int64(7)).
		WillReturnRows(pgxmock.NewRows(imageRowColumns).
			AddRow(int64(7), "ubuntu-24.04", "ubuntu", "https://example.com/ubuntu.img", testTime))

	repo := NewImageRepository(mock)
	img, err := repo.Get(context.Background(), 7)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if img.ID != 7 || img.DownloadURL != "https://example.com/ubuntu.img" {
		t.Fatalf("img = %+v, want id 7 with download_url", img)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestImageGetNoRows 钉死不存在 id 返回 pgx.ErrNoRows，供服务层映射 404。
func TestImageGetNoRows(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT " + imageCols + " FROM images WHERE id=$1").
		WithArgs(int64(999)).WillReturnError(pgx.ErrNoRows)

	repo := NewImageRepository(mock)
	_, err := repo.Get(context.Background(), 999)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("err = %v, want pgx.ErrNoRows", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestImageGetByName 验证按名称读取（幂等检查路径）：download_url
// 一并返回。
func TestImageGetByName(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT " + imageCols + " FROM images WHERE name=$1").
		WithArgs("ubuntu-24.04").
		WillReturnRows(pgxmock.NewRows(imageRowColumns).
			AddRow(int64(7), "ubuntu-24.04", "ubuntu", "https://example.com/ubuntu.img", testTime))

	repo := NewImageRepository(mock)
	img, err := repo.GetByName(context.Background(), "ubuntu-24.04")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if img.ID != 7 || img.Name != "ubuntu-24.04" || img.DownloadURL != "https://example.com/ubuntu.img" {
		t.Fatalf("img = %+v, want id 7 / name / download_url", img)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestImageList 验证全量列表：按 id 排序，download_url 正确映射。
func TestImageList(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT " + imageCols + " FROM images ORDER BY id").
		WillReturnRows(pgxmock.NewRows(imageRowColumns).
			AddRow(int64(7), "ubuntu-24.04", "ubuntu", "https://example.com/ubuntu.img", testTime).
			AddRow(int64(8), "debian-12", "debian", "https://example.com/debian.img", testTime.Add(1)))

	repo := NewImageRepository(mock)
	images, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(images) != 2 || images[0].ID != 7 || images[1].DownloadURL != "https://example.com/debian.img" {
		t.Fatalf("images = %+v, want 2 rows id 7 / 8", images)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestImageListPage 验证分页查询：LIMIT/OFFSET 参数与 download_url 映射。
func TestImageListPage(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT " + imageCols + " FROM images ORDER BY id LIMIT $1 OFFSET $2").
		WithArgs(10, 20).
		WillReturnRows(pgxmock.NewRows(imageRowColumns).
			AddRow(int64(7), "ubuntu-24.04", "ubuntu", "https://example.com/ubuntu.img", testTime))

	repo := NewImageRepository(mock)
	images, err := repo.ListPage(context.Background(), 10, 20)
	if err != nil {
		t.Fatalf("ListPage: %v", err)
	}
	if len(images) != 1 || images[0].ID != 7 || images[0].DownloadURL != "https://example.com/ubuntu.img" {
		t.Fatalf("images = %+v, want single row id 7", images)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
