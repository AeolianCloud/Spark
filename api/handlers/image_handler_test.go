package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"spark/api/middleware"
	"spark/model"
	"spark/pve"
	"spark/repository"
	"spark/service"
)

// imageTestTime 是镜像 handler 测试使用的固定时间戳。
var imageTestTime = time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)

// imageRowCols 是 images 表读取列（与 repository.imageCols 一致）的扫描顺序，
// 供 pgxmock 行构造使用。
var imageRowCols = []string{"id", "name", "default_user", "download_url", "created_at"}

// newImageMockPool 构建用于 *repository.ImageRepository 的 pgxmock 池。
func newImageMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return mock
}

// fakeImageNodeRepo 是 handler 测试的 ImageNodeRepository 替身：按预置节点
// 列表提供节点查询与区域启用节点查询。
type fakeImageNodeRepo struct {
	nodes []model.PVENode
}

func (f *fakeImageNodeRepo) GetNode(ctx context.Context, id int64) (*model.PVENode, error) {
	for i := range f.nodes {
		if f.nodes[i].ID == id {
			n := f.nodes[i]
			return &n, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (f *fakeImageNodeRepo) ListEnabledNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error) {
	out := make([]model.PVENode, 0)
	for _, n := range f.nodes {
		if n.ZoneID == zoneID && n.Enabled {
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *fakeImageNodeRepo) ListAllEnabledNodes(ctx context.Context) ([]model.PVENode, error) {
	out := make([]model.PVENode, 0)
	for _, n := range f.nodes {
		if n.Enabled {
			out = append(out, n)
		}
	}
	return out, nil
}

// fakeImageOpRepo 是 handler 测试的 ImageOperationRepository 替身：操作记录
// 保存在内存切片中（下载 goroutine 写终态、测试读快照，因此并发安全）。
type fakeImageOpRepo struct {
	mu  sync.Mutex
	ops []model.ImageOperation
}

func (f *fakeImageOpRepo) CreateOperation(ctx context.Context, op model.ImageOperation) (*model.ImageOperation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	op.ID = int64(len(f.ops) + 1)
	f.ops = append(f.ops, op)
	created := f.ops[len(f.ops)-1]
	return &created, nil
}

func (f *fakeImageOpRepo) UpdateOperationResult(ctx context.Context, id int64, result, errorMessage, upid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.ops {
		if f.ops[i].ID == id {
			f.ops[i].Result = result
			f.ops[i].ErrorMessage = errorMessage
			f.ops[i].UPID = upid
		}
	}
	return nil
}

func (f *fakeImageOpRepo) ListOperationsByImage(ctx context.Context, imageID int64, limit, offset int) ([]model.ImageOperation, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	filtered := make([]model.ImageOperation, 0)
	for _, op := range f.ops {
		if op.ImageID == imageID {
			filtered = append(filtered, op)
		}
	}
	reversed := make([]model.ImageOperation, 0, len(filtered))
	for i := len(filtered) - 1; i >= 0; i-- {
		reversed = append(reversed, filtered[i])
	}
	start := min(offset, len(reversed))
	end := min(start+limit, len(reversed))
	return reversed[start:end], len(reversed), nil
}

// HasRunningOperation 报告镜像在节点上是否已有 running 记录（幂等检查，
// 与 service.ImageOperationRepository 接口同步）。
func (f *fakeImageOpRepo) HasRunningOperation(ctx context.Context, imageID, nodeID int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, op := range f.ops {
		if op.ImageID == imageID && op.NodeID == nodeID && op.Result == model.ImageOpResultRunning {
			return true, nil
		}
	}
	return false, nil
}

// snapshot 返回当前全部操作的拷贝（断言用）。
func (f *fakeImageOpRepo) snapshot() []model.ImageOperation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.ImageOperation(nil), f.ops...)
}

// fixedImageUPID 是假 PVE 服务器受理下载返回的固定任务 ID。
const fixedImageUPID = "UPID:aeolian1:00000001:00000002:5FAB1EC4:imgcopy:1:root@pam:"

// handlerImagePVE 是 handler 测试的假 PVE 服务器：content 扫描返回可配置的
// 文件清单（键为节点 PVE 名），download-url 受理返回固定 UPID，任务状态
// 返回成功。
type handlerImagePVE struct {
	contents map[string][]pve.StorageContent
}

func (s *handlerImagePVE) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		// 期望形如 /nodes/{node}/storage/local/content、.../download-url 或
		// /nodes/{node}/tasks/{upid}/status。
		if len(parts) != 5 || parts[0] != "nodes" {
			http.NotFound(w, r)
			return
		}
		node := parts[1]
		switch {
		case parts[2] == "storage" && parts[3] == "local" && parts[4] == "content":
			items := s.contents[node]
			if items == nil {
				items = []pve.StorageContent{}
			}
			writeHandlerPVEData(w, items)
		case parts[2] == "storage" && parts[3] == "local" && parts[4] == "download-url" && r.Method == http.MethodPost:
			writeHandlerPVEData(w, fixedImageUPID)
		case parts[2] == "tasks" && parts[4] == "status" && r.Method == http.MethodGet:
			writeHandlerPVEData(w, map[string]string{"status": "stopped", "exitstatus": "OK", "upid": parts[3]})
		default:
			http.NotFound(w, r)
		}
	})
	return mux
}

func writeHandlerPVEData(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// newHandlerImagePVEServer 启动带可配置文件清单的假 PVE 服务器。
func newHandlerImagePVEServer(t *testing.T, contents map[string][]pve.StorageContent) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer((&handlerImagePVE{contents: contents}).handler())
	t.Cleanup(ts.Close)
	return ts
}

// newImageTestService 装配 handler 测试的 ImageService：pgxmock 镜像仓库
// （调用方需预先注册 SQL 期望）、fake 节点/操作仓库与假 PVE 服务器。
// 测试用例的 download_url 使用 cloud.example.com（非内置默认白名单域名），
// 因此显式注入白名单，避免镜像下载源校验（M1）拦截用例。
func newImageTestService(t *testing.T, mock pgxmock.PgxPoolIface, nodeRepo *fakeImageNodeRepo, opRepo *fakeImageOpRepo, srv *httptest.Server) *service.ImageService {
	t.Helper()
	svc := service.NewImageService(repository.NewImageRepository(mock), nodeRepo, opRepo)
	svc.SetDownloadHostAllowlist([]string{"cloud.example.com"})
	svc.SetClientFactory(func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient(host, apiUser, apiTokenSecret,
			pve.WithBaseURL(srv.URL), pve.WithHTTPClient(srv.Client()), pve.WithTimeout(5*time.Second))
	})
	return svc
}

// newImageHandlerEngine 构建挂载 /images 分组的 gin 引擎。
func newImageHandlerEngine(svc *service.ImageService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterImagesRoutes(r.Group("/images"), svc)
	return r
}

// waitForImageOpsDone 阻塞直到 opRepo 中全部操作都不是 running（下载
// goroutine 异步写终态，测试必须等待）或短暂超时。
func waitForImageOpsDone(t *testing.T, opRepo *fakeImageOpRepo) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ops := opRepo.snapshot()
		allDone := len(ops) > 0
		for _, op := range ops {
			if op.Result == model.ImageOpResultRunning {
				allDone = false
				break
			}
		}
		if allDone {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for image downloads to finish: %+v", opRepo.snapshot())
}

// TestImageCreateHandler 覆盖 POST /images：合法请求 201 + Location；空
// body 400；download_url 缺失由 service 校验并映射 400。
func TestImageCreateHandler(t *testing.T) {
	const (
		imgID    = int64(7)
		name     = "debian-12-cloud"
		user     = "debian"
		download = "https://cloud.example.com/debian-12-cloud.qcow2"
	)

	t.Run("creates image with 201 and location", func(t *testing.T) {
		mock := newImageMockPool(t)
		mock.ExpectQuery("INSERT INTO images \\(name, default_user, download_url\\) VALUES \\(\\$1, \\$2, \\$3\\) RETURNING id, created_at").
			WithArgs(name, user, download).
			WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).AddRow(imgID, imageTestTime))
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, &fakeImageOpRepo{}, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/images",
			strings.NewReader(`{"name":"debian-12-cloud","default_user":"debian","download_url":"https://cloud.example.com/debian-12-cloud.qcow2"}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201, body: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Location"); got != "/images/7" {
			t.Errorf("Location = %q, want /images/7", got)
		}
		var img model.Image
		if err := json.Unmarshal(w.Body.Bytes(), &img); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if img.ID != imgID || img.Name != name || img.DefaultUser != user || img.DownloadURL != download {
			t.Errorf("img = %+v, want id %d name %q", img, imgID, name)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("empty body is rejected with 400", func(t *testing.T) {
		mock := newImageMockPool(t)
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, &fakeImageOpRepo{}, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/images", nil)
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
		if got := w.Header().Get(middleware.XMSErrorCodeHeader); got != CodeBadRequest {
			t.Errorf("x-ms-error-code = %q, want %q", got, CodeBadRequest)
		}
	})

	t.Run("missing download_url maps service validation to 400", func(t *testing.T) {
		mock := newImageMockPool(t)
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, &fakeImageOpRepo{}, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/images",
			strings.NewReader(`{"name":"debian-12-cloud","default_user":"debian"}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get(middleware.XMSErrorCodeHeader); got != CodeBadRequest {
			t.Errorf("x-ms-error-code = %q, want %q", got, CodeBadRequest)
		}
	})
}

// TestImageListHandler 覆盖 GET /images 的两个分支：不带 zone_id 返回全量
// 镜像分页；带 zone_id 返回 ImageZoneItem（image + nodes）并过滤区域。
func TestImageListHandler(t *testing.T) {
	const (
		debianURL = "https://cloud.example.com/debian.qcow2"
		ubuntuURL = "https://cloud.example.com/ubuntu.qcow2"
	)
	images := func() *pgxmock.Rows {
		return pgxmock.NewRows(imageRowCols).
			AddRow(int64(1), "debian-12-cloud", "debian", debianURL, imageTestTime).
			AddRow(int64(2), "ubuntu-24-cloud", "ubuntu", ubuntuURL, imageTestTime)
	}
	expectZone := func(mock pgxmock.PgxPoolIface, zoneID int64, exists bool) {
		mock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM zones WHERE id=\\$1\\)").
			WithArgs(zoneID).
			WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(exists))
	}

	t.Run("without zone returns all images with total count", func(t *testing.T) {
		mock := newImageMockPool(t)
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images ORDER BY id LIMIT \\$1 OFFSET \\$2").
			WithArgs(25, 0).WillReturnRows(images())
		mock.ExpectQuery("SELECT count\\(\\*\\) FROM images").
			WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(2))
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, &fakeImageOpRepo{}, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/images", nil))

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get(XTotalCountHeader); got != "2" {
			t.Errorf("X-Total-Count = %q, want 2", got)
		}
		var got []model.Image
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
			t.Errorf("got %+v, want both images in id order", got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("with zone returns zone items with node statuses", func(t *testing.T) {
		mock := newImageMockPool(t)
		expectZone(mock, 1, true)
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images ORDER BY id$").
			WillReturnRows(images())
		nodeRepo := &fakeImageNodeRepo{nodes: []model.PVENode{
			{ID: 1, ZoneID: 1, Name: "pve1", PveName: "aeolian1", Host: "10.0.0.1", Port: 8006, APIUser: "root@pam!spark", APITokenSecret: "s1", Enabled: true},
		}}
		// debian 已在节点 aeolian1 上下载；ubuntu 没有。
		srv := newHandlerImagePVEServer(t, map[string][]pve.StorageContent{
			"aeolian1": {{VolID: "local:import/debian.qcow2", Name: "debian.qcow2"}},
		})
		svc := newImageTestService(t, mock, nodeRepo, &fakeImageOpRepo{}, srv)
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/images?zone_id=1", nil))

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get(XTotalCountHeader); got != "1" {
			t.Errorf("X-Total-Count = %q, want 1", got)
		}
		var got []service.ImageZoneItem
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d items, want 1", len(got))
		}
		item := got[0]
		if item.Image.Name != "debian-12-cloud" || item.Image.ID != 1 {
			t.Errorf("item.Image = %+v, want debian-12-cloud", item.Image)
		}
		if len(item.Nodes) != 1 || item.Nodes[0].NodeID != 1 || !item.Nodes[0].Downloaded ||
			item.Nodes[0].VolID != "local:import/debian.qcow2" {
			t.Errorf("item.Nodes = %+v, want node 1 downloaded", item.Nodes)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("missing zone maps to 404", func(t *testing.T) {
		mock := newImageMockPool(t)
		expectZone(mock, 9, false)
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, &fakeImageOpRepo{}, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/images?zone_id=9", nil))

		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404, body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("invalid zone_id rejected", func(t *testing.T) {
		mock := newImageMockPool(t)
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, &fakeImageOpRepo{}, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		for _, q := range []string{"abc", "0", "-1"} {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/images?zone_id="+q, nil))
			if w.Code != http.StatusBadRequest {
				t.Errorf("zone_id=%q status = %d, want 400", q, w.Code)
			}
		}
	})
}

// TestImageNodeStatusHandler 覆盖 GET /images/:id/nodes-status：正常 200、
// zone_id 限定扫描范围、非法 id/zone_id 400、镜像不存在 404。
func TestImageNodeStatusHandler(t *testing.T) {
	const (
		imgID     = int64(7)
		debianURL = "https://cloud.example.com/debian.qcow2"
	)
	expectImage := func(mock pgxmock.PgxPoolIface, id int64) {
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images WHERE id=\\$1").
			WithArgs(id).
			WillReturnRows(pgxmock.NewRows(imageRowCols).AddRow(id, "debian-12-cloud", "debian", debianURL, imageTestTime))
	}
	expectZone := func(mock pgxmock.PgxPoolIface, zoneID int64, exists bool) {
		mock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM zones WHERE id=\\$1\\)").
			WithArgs(zoneID).
			WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(exists))
	}
	// 标准节点集：zone 1 启用 pve1（aeolian1）、zone 2 启用 pve4（aeolian4）。
	nodes := []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", PveName: "aeolian1", Host: "10.0.0.1", Port: 8006, APIUser: "root@pam!spark", APITokenSecret: "s1", Enabled: true},
		{ID: 4, ZoneID: 2, Name: "pve4", PveName: "aeolian4", Host: "10.0.0.4", Port: 8006, APIUser: "root@pam!spark", APITokenSecret: "s4", Enabled: true},
	}

	t.Run("returns per-node status across all enabled nodes", func(t *testing.T) {
		mock := newImageMockPool(t)
		expectImage(mock, imgID)
		srv := newHandlerImagePVEServer(t, map[string][]pve.StorageContent{
			"aeolian1": {{VolID: "local:import/debian.qcow2", Name: "debian.qcow2"}},
			"aeolian4": {},
		})
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{nodes: nodes}, &fakeImageOpRepo{}, srv)
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/images/7/nodes-status", nil))

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
		}
		var got []service.NodeImageStatus
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d statuses, want 2", len(got))
		}
		if got[0].NodeID != 1 || !got[0].Downloaded || got[0].VolID != "local:import/debian.qcow2" {
			t.Errorf("got[0] = %+v, want node 1 downloaded", got[0])
		}
		if got[1].NodeID != 4 || got[1].Downloaded {
			t.Errorf("got[1] = %+v, want node 4 not downloaded", got[1])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("scopes scan to zone when zone_id given", func(t *testing.T) {
		mock := newImageMockPool(t)
		expectImage(mock, imgID)
		expectZone(mock, 1, true)
		srv := newHandlerImagePVEServer(t, nil)
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{nodes: nodes}, &fakeImageOpRepo{}, srv)
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/images/7/nodes-status?zone_id=1", nil))

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
		}
		var got []service.NodeImageStatus
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if len(got) != 1 || got[0].NodeID != 1 {
			t.Errorf("got %+v, want only zone 1 node", got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("missing image maps to 404", func(t *testing.T) {
		mock := newImageMockPool(t)
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images WHERE id=\\$1").
			WithArgs(int64(99)).WillReturnError(pgx.ErrNoRows)
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, &fakeImageOpRepo{}, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/images/99/nodes-status", nil))

		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404, body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("invalid id rejected", func(t *testing.T) {
		mock := newImageMockPool(t)
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, &fakeImageOpRepo{}, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/images/abc/nodes-status", nil))

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("invalid zone_id rejected", func(t *testing.T) {
		mock := newImageMockPool(t)
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, &fakeImageOpRepo{}, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		for _, q := range []string{"abc", "0", "-1"} {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/images/7/nodes-status?zone_id="+q, nil))
			if w.Code != http.StatusBadRequest {
				t.Errorf("zone_id=%q status = %d, want 400", q, w.Code)
			}
		}
	})
}

// TestImageDownloadHandler 覆盖 POST /images/:id/download：202 + Location +
// operations 数组；镜像不存在 404；zone_id 与 node_ids 同时提供 400；空
// body 400；无目标节点 400。
func TestImageDownloadHandler(t *testing.T) {
	const (
		imgID     = int64(7)
		debianURL = "https://cloud.example.com/debian.qcow2"
	)
	expectImage := func(mock pgxmock.PgxPoolIface, id int64) {
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images WHERE id=\\$1").
			WithArgs(id).
			WillReturnRows(pgxmock.NewRows(imageRowCols).AddRow(id, "debian-12-cloud", "debian", debianURL, imageTestTime))
	}
	expectZone := func(mock pgxmock.PgxPoolIface, zoneID int64, exists bool) {
		mock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM zones WHERE id=\\$1\\)").
			WithArgs(zoneID).
			WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(exists))
	}
	// 下载目标节点：zone 1 启用 pve1（aeolian1），host/port/凭据供假 PVE 使用。
	nodes := []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", PveName: "aeolian1", Host: "10.0.0.1", Port: 8006, APIUser: "root@pam!spark", APITokenSecret: "s1", Enabled: true},
	}

	t.Run("accepts node download with 202, location and running operations", func(t *testing.T) {
		mock := newImageMockPool(t)
		expectImage(mock, imgID)
		opRepo := &fakeImageOpRepo{}
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{nodes: nodes}, opRepo, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/images/7/download",
			strings.NewReader(`{"node_ids":[1]}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202, body: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Location"); got != "/images/7/operations" {
			t.Errorf("Location = %q, want /images/7/operations", got)
		}
		var ops []model.ImageOperation
		if err := json.Unmarshal(w.Body.Bytes(), &ops); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if len(ops) != 1 {
			t.Fatalf("got %d operations, want 1", len(ops))
		}
		op := ops[0]
		if op.ID != 1 || op.ImageID != imgID || op.NodeID != 1 ||
			op.Action != model.ImageOpActionDownload || op.Result != model.ImageOpResultRunning {
			t.Errorf("op = %+v, want running download on node 1", op)
		}
		// 后台 goroutine 异步写终态，等待其完成后校验 SQL 期望全部满足。
		waitForImageOpsDone(t, opRepo)
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("running duplicate rejected with 409 conflict", func(t *testing.T) {
		mock := newImageMockPool(t)
		expectImage(mock, imgID)
		// 预置该镜像在节点 1 上未终态的下载记录，触发幂等拒绝。
		opRepo := &fakeImageOpRepo{ops: []model.ImageOperation{
			{ID: 9, ImageID: imgID, NodeID: 1, Action: model.ImageOpActionDownload, Result: model.ImageOpResultRunning},
		}}
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{nodes: nodes}, opRepo, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/images/7/download",
			strings.NewReader(`{"node_ids":[1]}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409, body: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get(middleware.XMSErrorCodeHeader); got != CodeConflict {
			t.Errorf("x-ms-error-code = %q, want %q", got, CodeConflict)
		}
	})

	t.Run("zone mode accepts download to zone nodes", func(t *testing.T) {
		mock := newImageMockPool(t)
		expectImage(mock, imgID)
		expectZone(mock, 1, true)
		opRepo := &fakeImageOpRepo{}
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{nodes: nodes}, opRepo, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/images/7/download",
			strings.NewReader(`{"zone_id":1}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202, body: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Location"); got != "/images/7/operations" {
			t.Errorf("Location = %q, want /images/7/operations", got)
		}
		var ops []model.ImageOperation
		if err := json.Unmarshal(w.Body.Bytes(), &ops); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if len(ops) != 1 || ops[0].NodeID != 1 {
			t.Errorf("ops = %+v, want one operation on node 1", ops)
		}
		waitForImageOpsDone(t, opRepo)
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("missing image maps to 404", func(t *testing.T) {
		mock := newImageMockPool(t)
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images WHERE id=\\$1").
			WithArgs(int64(99)).WillReturnError(pgx.ErrNoRows)
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, &fakeImageOpRepo{}, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/images/99/download",
			strings.NewReader(`{"node_ids":[1]}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404, body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("zone_id and node_ids together rejected", func(t *testing.T) {
		mock := newImageMockPool(t)
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, &fakeImageOpRepo{}, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/images/7/download",
			strings.NewReader(`{"node_ids":[1],"zone_id":1}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get(middleware.XMSErrorCodeHeader); got != CodeBadRequest {
			t.Errorf("x-ms-error-code = %q, want %q", got, CodeBadRequest)
		}
	})

	t.Run("invalid zone_id rejected", func(t *testing.T) {
		mock := newImageMockPool(t)
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, &fakeImageOpRepo{}, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/images/7/download",
			strings.NewReader(`{"zone_id":0}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get(middleware.XMSErrorCodeHeader); got != CodeBadRequest {
			t.Errorf("x-ms-error-code = %q, want %q", got, CodeBadRequest)
		}
	})

	t.Run("negative zone_id rejected", func(t *testing.T) {
		mock := newImageMockPool(t)
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, &fakeImageOpRepo{}, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/images/7/download",
			strings.NewReader(`{"zone_id":-1}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get(middleware.XMSErrorCodeHeader); got != CodeBadRequest {
			t.Errorf("x-ms-error-code = %q, want %q", got, CodeBadRequest)
		}
	})

	t.Run("no target nodes rejected", func(t *testing.T) {
		mock := newImageMockPool(t)
		expectImage(mock, imgID)
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, &fakeImageOpRepo{}, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/images/7/download",
			strings.NewReader(`{"node_ids":[]}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("invalid body rejected", func(t *testing.T) {
		mock := newImageMockPool(t)
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, &fakeImageOpRepo{}, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/images/7/download", nil)
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body: %s", w.Code, w.Body.String())
		}
	})
}

// TestImageGetHandler 覆盖 GET /images/:id：正常 200 返回镜像元数据；
// 镜像不存在 404；非法 id 400。
func TestImageGetHandler(t *testing.T) {
	const (
		imgID     = int64(7)
		debianURL = "https://cloud.example.com/debian.qcow2"
	)

	t.Run("returns image metadata", func(t *testing.T) {
		mock := newImageMockPool(t)
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images WHERE id=\\$1").
			WithArgs(imgID).
			WillReturnRows(pgxmock.NewRows(imageRowCols).AddRow(imgID, "debian-12-cloud", "debian", debianURL, imageTestTime))
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, &fakeImageOpRepo{}, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/images/7", nil))

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
		}
		var img model.Image
		if err := json.Unmarshal(w.Body.Bytes(), &img); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if img.ID != imgID || img.Name != "debian-12-cloud" || img.DefaultUser != "debian" || img.DownloadURL != debianURL {
			t.Errorf("img = %+v, want id %d debian-12-cloud", img, imgID)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("missing image maps to 404", func(t *testing.T) {
		mock := newImageMockPool(t)
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images WHERE id=\\$1").
			WithArgs(int64(99)).WillReturnError(pgx.ErrNoRows)
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, &fakeImageOpRepo{}, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/images/99", nil))

		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404, body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("invalid id rejected", func(t *testing.T) {
		mock := newImageMockPool(t)
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, &fakeImageOpRepo{}, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/images/abc", nil))

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get(middleware.XMSErrorCodeHeader); got != CodeBadRequest {
			t.Errorf("x-ms-error-code = %q, want %q", got, CodeBadRequest)
		}
	})
}

// TestImageOperationsHandler 覆盖 GET /images/:id/operations：分页 + 总数
// 头；镜像不存在 404；非法 id 400。
func TestImageOperationsHandler(t *testing.T) {
	const (
		imgID     = int64(7)
		debianURL = "https://cloud.example.com/debian.qcow2"
	)
	expectImage := func(mock pgxmock.PgxPoolIface, id int64) {
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images WHERE id=\\$1").
			WithArgs(id).
			WillReturnRows(pgxmock.NewRows(imageRowCols).AddRow(id, "debian-12-cloud", "debian", debianURL, imageTestTime))
	}

	t.Run("returns paged operations with total count", func(t *testing.T) {
		mock := newImageMockPool(t)
		expectImage(mock, imgID)
		// 操作记录由 fake 仓库预置（handler 测试不经过 pgxmock 的
		// image_operations 查询，SQL 匹配由 service 层测试覆盖）。
		opRepo := &fakeImageOpRepo{ops: []model.ImageOperation{
			{ID: 1, ImageID: imgID, NodeID: 2, Action: model.ImageOpActionDownload, Result: model.ImageOpResultFailed, ErrorMessage: "storage content scan boom"},
			{ID: 2, ImageID: imgID, NodeID: 1, Action: model.ImageOpActionDownload, Result: model.ImageOpResultSuccess, UPID: fixedImageUPID},
		}}
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, opRepo, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/images/7/operations", nil))

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get(XTotalCountHeader); got != "2" {
			t.Errorf("X-Total-Count = %q, want 2", got)
		}
		var ops []model.ImageOperation
		if err := json.Unmarshal(w.Body.Bytes(), &ops); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if len(ops) != 2 {
			t.Fatalf("got %d operations, want 2", len(ops))
		}
		if ops[0].ID != 2 || ops[0].Result != model.ImageOpResultSuccess || ops[0].ErrorMessage != "" {
			t.Errorf("ops[0] = %+v, want success with empty error", ops[0])
		}
		if ops[1].ID != 1 || ops[1].Result != model.ImageOpResultFailed || ops[1].ErrorMessage == "" {
			t.Errorf("ops[1] = %+v, want failed with error message", ops[1])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("missing image maps to 404", func(t *testing.T) {
		mock := newImageMockPool(t)
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images WHERE id=\\$1").
			WithArgs(int64(99)).WillReturnError(pgx.ErrNoRows)
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, &fakeImageOpRepo{}, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/images/99/operations", nil))

		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404, body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("invalid id rejected", func(t *testing.T) {
		mock := newImageMockPool(t)
		svc := newImageTestService(t, mock, &fakeImageNodeRepo{}, &fakeImageOpRepo{}, newHandlerImagePVEServer(t, nil))
		r := newImageHandlerEngine(svc)

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/images/abc/operations", nil))

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body: %s", w.Code, w.Body.String())
		}
	})
}
