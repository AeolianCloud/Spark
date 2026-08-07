package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"spark/model"
	"spark/pve"
	"spark/repository"
)

// imageTestTime 是镜像服务测试使用的固定时间戳。
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

// imageTestNodes 是镜像服务测试的标准节点集：zone 1 的启用节点 pve1
// （PveName=aeolian1）与 pve2（PveName 空，PVE API 路径回退业务名）、zone 1
// 的禁用节点 pve3，以及 zone 2 的启用节点 pve4。
func imageTestNodes() []model.PVENode {
	return []model.PVENode{
		{ID: 1, ZoneID: 1, Name: "pve1", PveName: "aeolian1", Host: "10.0.0.1", Port: 8006, APIUser: "root@pam!spark", APITokenSecret: "s1", Enabled: true},
		{ID: 2, ZoneID: 1, Name: "pve2", Host: "10.0.0.2", Port: 8006, APIUser: "root@pam!spark", APITokenSecret: "s2", Enabled: true},
		{ID: 3, ZoneID: 1, Name: "pve3", Host: "10.0.0.3", Port: 8006, APIUser: "root@pam!spark", APITokenSecret: "s3", Enabled: false},
		{ID: 4, ZoneID: 2, Name: "pve4", PveName: "aeolian4", Host: "10.0.0.4", Port: 8006, APIUser: "root@pam!spark", APITokenSecret: "s4", Enabled: true},
	}
}

// fakeImageNodeRepository 是供服务测试使用的可脚本化 ImageNodeRepository。
type fakeImageNodeRepository struct {
	nodes []model.PVENode
	err   error
}

func (f *fakeImageNodeRepository) GetNode(ctx context.Context, id int64) (*model.PVENode, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.nodes {
		if f.nodes[i].ID == id {
			n := f.nodes[i]
			return &n, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (f *fakeImageNodeRepository) ListEnabledNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]model.PVENode, 0)
	for _, n := range f.nodes {
		if n.ZoneID == zoneID && n.Enabled {
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *fakeImageNodeRepository) ListAllEnabledNodes(ctx context.Context) ([]model.PVENode, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]model.PVENode, 0)
	for _, n := range f.nodes {
		if n.Enabled {
			out = append(out, n)
		}
	}
	return out, nil
}

// fakeImageOperationRepository 是供服务测试使用的可脚本化
// ImageOperationRepository（goroutine 写终态、测试读快照，因此并发安全）。
type fakeImageOperationRepository struct {
	mu        sync.Mutex
	ops       []model.ImageOperation
	createErr error
	updateErr error
	listErr   error
}

func newFakeImageOperationRepository() *fakeImageOperationRepository {
	return &fakeImageOperationRepository{}
}

func (f *fakeImageOperationRepository) CreateOperation(ctx context.Context, op model.ImageOperation) (*model.ImageOperation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	op.ID = int64(len(f.ops) + 1)
	f.ops = append(f.ops, op)
	created := f.ops[len(f.ops)-1]
	return &created, nil
}

func (f *fakeImageOperationRepository) UpdateOperationResult(ctx context.Context, id int64, result, errorMessage, upid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateErr != nil {
		return f.updateErr
	}
	for i := range f.ops {
		if f.ops[i].ID == id {
			f.ops[i].Result = result
			f.ops[i].ErrorMessage = errorMessage
			f.ops[i].UPID = upid
		}
	}
	return nil
}

func (f *fakeImageOperationRepository) ListOperationsByImage(ctx context.Context, imageID int64, limit, offset int) ([]model.ImageOperation, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	filtered := make([]model.ImageOperation, 0)
	for _, op := range f.ops {
		if op.ImageID == imageID {
			filtered = append(filtered, op)
		}
	}
	// 与真实仓库一致按时间倒序（fake 中按 id 倒序）。
	reversed := make([]model.ImageOperation, 0, len(filtered))
	for i := len(filtered) - 1; i >= 0; i-- {
		reversed = append(reversed, filtered[i])
	}
	start := min(offset, len(reversed))
	end := min(start+limit, len(reversed))
	return reversed[start:end], len(reversed), nil
}

// HasRunningOperation 报告镜像在节点上是否已有 running 记录（幂等检查）。
func (f *fakeImageOperationRepository) HasRunningOperation(ctx context.Context, imageID, nodeID int64) (bool, error) {
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
func (f *fakeImageOperationRepository) snapshot() []model.ImageOperation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.ImageOperation(nil), f.ops...)
}

// scriptedImagePVE 是可脚本化的假 PVE 服务器：按节点预置 local/import 的
// 文件清单（contents）、download-url 受理结果（UPID 或注入错误）与任务轮询
// 终态（成功或注入错误），并记录受理过的节点供断言。
type scriptedImagePVE struct {
	mu          sync.Mutex
	contents    map[string][]pve.StorageContent // 键：节点 PVE 名
	contentErr  map[string]bool                 // 键：节点 PVE 名 -> content 扫描返回错误
	upids       map[string]string               // 键：节点 PVE 名 -> download-url 返回的 UPID
	downloadErr map[string]bool                 // 键：节点 PVE 名 -> download-url 受理失败
	taskErr     map[string]bool                 // 键：UPID -> 任务轮询失败
	downloads   []string                        // 已受理下载的节点 PVE 名（断言用）
}

func newScriptedImagePVE() *scriptedImagePVE {
	return &scriptedImagePVE{
		contents:    map[string][]pve.StorageContent{},
		contentErr:  map[string]bool{},
		upids:       map[string]string{},
		downloadErr: map[string]bool{},
		taskErr:     map[string]bool{},
	}
}

func (s *scriptedImagePVE) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
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
			if s.contentErr[node] {
				writePVEError(w, http.StatusInternalServerError, "storage", "content scan boom")
				return
			}
			items := s.contents[node]
			if items == nil {
				items = []pve.StorageContent{}
			}
			writePVEData(w, items)
		case parts[2] == "storage" && parts[3] == "local" && parts[4] == "download-url" && r.Method == http.MethodPost:
			if s.downloadErr[node] {
				writePVEError(w, http.StatusBadRequest, "url", "download-url rejected")
				return
			}
			upid := s.upids[node]
			if upid == "" {
				upid = fmt.Sprintf("UPID:%s:00000001:00000002:5FAB1EC4:imgcopy:1:root@pam:", node)
				s.upids[node] = upid
			}
			s.downloads = append(s.downloads, node)
			writePVEData(w, upid)
		case parts[2] == "tasks" && parts[4] == "status" && r.Method == http.MethodGet:
			upid := parts[3]
			if s.taskErr[upid] {
				writePVEError(w, http.StatusInternalServerError, "task", "task status boom")
				return
			}
			writePVEData(w, map[string]string{"status": "stopped", "exitstatus": "OK", "upid": upid})
		default:
			http.NotFound(w, r)
		}
	})
	return mux
}

// receivedDownloads 返回已受理 download-url 的节点 PVE 名拷贝（断言用）。
func (s *scriptedImagePVE) receivedDownloads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.downloads...)
}

func writePVEData(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func writePVEError(w http.ResponseWriter, status int, key, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": map[string]string{key: message}})
}

// newImageTestService 用 pgxmock 镜像仓库（调用方需预先注册 SQL 期望）、
// fake 节点/操作仓库与脚本化假 PVE 服务器装配 ImageService；SetClientFactory
// 指向假服务器。
func newImageTestService(t *testing.T, mock pgxmock.PgxPoolIface, nodeRepo *fakeImageNodeRepository, opRepo *fakeImageOperationRepository, srv *scriptedImagePVE) *ImageService {
	t.Helper()
	svc := NewImageService(repository.NewImageRepository(mock), nodeRepo, opRepo)
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	svc.SetClientFactory(func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient(host, apiUser, apiTokenSecret,
			pve.WithBaseURL(ts.URL), pve.WithHTTPClient(ts.Client()), pve.WithTimeout(5*time.Second))
	})
	return svc
}

// waitForImageDownloads 阻塞直到 opRepo 中全部操作都不是 running（下载
// goroutine 异步写终态，测试必须等待）或短暂超时。
func waitForImageDownloads(t *testing.T, opRepo *fakeImageOperationRepository) {
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

// assertServiceErrorKind 断言 err 是 service.Error 且 kind 匹配。
func assertServiceErrorKind(t *testing.T, err error, kind ErrorKind) {
	t.Helper()
	var se *Error
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *service.Error", err)
	}
	if se.Kind != kind {
		t.Fatalf("err kind = %v, want %v (msg %q)", se.Kind, kind, se.Message)
	}
}

// ---------- Create ----------

// TestCreateImageValidatesDownloadURL 验证镜像创建的 download_url 校验：
// 空值、非 http(s) 协议、不可解析 URL、host 不在下载源白名单、文件名非法
// 一律 bad_request；白名单内合法 URL 走仓库落库。
func TestCreateImageValidatesDownloadURL(t *testing.T) {
	const (
		name        = "ubuntu-24.04"
		defaultUser = "ubuntu"
	)
	cases := []struct {
		name        string
		downloadURL string
		wantErr     bool
	}{
		{name: "empty url", downloadURL: "   ", wantErr: true},
		{name: "non-http scheme", downloadURL: "ftp://mirror.example.com/debian.qcow2", wantErr: true},
		{name: "http scheme without host", downloadURL: "https://", wantErr: true},
		{name: "unparsable url", downloadURL: "://nope", wantErr: true},
		{name: "host not in allowlist", downloadURL: "https://evil.example.com/debian.qcow2", wantErr: true},
		{name: "url without file name", downloadURL: "https://cloud.debian.org/", wantErr: true},
		{name: "url ending with parent dir", downloadURL: "https://cloud.debian.org/..", wantErr: true},
		{name: "valid https url", downloadURL: "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img", wantErr: false},
		{name: "allowlisted host with port", downloadURL: "https://cloud.debian.org:8443/cloud/debian.qcow2", wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newImageMockPool(t)
			if !tc.wantErr {
				mock.ExpectQuery("INSERT INTO images \\(name, default_user, download_url\\) VALUES \\(\\$1, \\$2, \\$3\\) RETURNING id, created_at").
					WithArgs(name, defaultUser, tc.downloadURL).
					WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).AddRow(int64(7), imageTestTime))
			}
			svc := newImageTestService(t, mock, &fakeImageNodeRepository{}, newFakeImageOperationRepository(), newScriptedImagePVE())
			img, err := svc.Create(context.Background(), name, defaultUser, tc.downloadURL)
			if tc.wantErr {
				assertServiceErrorKind(t, err, KindBadRequest)
				return
			}
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if img.ID != 7 || img.DownloadURL != tc.downloadURL {
				t.Fatalf("img = %+v, want id 7 with download_url", img)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet expectations: %v", err)
			}
		})
	}
}

// TestCreateImageRejectsAllWhenAllowlistEmpty 验证白名单为空（拒绝所有）的
// 语义：即使域名属于内置默认白名单也被拒绝，错误消息指出被拒 host。
func TestCreateImageRejectsAllWhenAllowlistEmpty(t *testing.T) {
	mock := newImageMockPool(t)
	svc := newImageTestService(t, mock, &fakeImageNodeRepository{}, newFakeImageOperationRepository(), newScriptedImagePVE())
	svc.SetDownloadHostAllowlist([]string{})
	_, err := svc.Create(context.Background(), "debian-12", "debian", "https://cloud.debian.org/cloud/debian.qcow2")
	assertServiceErrorKind(t, err, KindBadRequest)
	var se *Error
	errors.As(err, &se)
	if !strings.Contains(se.Message, "cloud.debian.org") || !strings.Contains(se.Message, "not allowlisted") {
		t.Fatalf("err message = %q, want to name the rejected host", se.Message)
	}
}

// TestCreateImageValidatesNameAndUser 验证名称与默认用户的非空校验仍然生效。
func TestCreateImageValidatesNameAndUser(t *testing.T) {
	mock := newImageMockPool(t)
	svc := newImageTestService(t, mock, &fakeImageNodeRepository{}, newFakeImageOperationRepository(), newScriptedImagePVE())
	if _, err := svc.Create(context.Background(), "  ", "debian", "https://example.com/d.img"); err == nil {
		t.Fatal("empty name: want error")
	} else {
		assertServiceErrorKind(t, err, KindBadRequest)
	}
	if _, err := svc.Create(context.Background(), "debian-12", "  ", "https://example.com/d.img"); err == nil {
		t.Fatal("empty default_user: want error")
	} else {
		assertServiceErrorKind(t, err, KindBadRequest)
	}
}

// ---------- 文件名匹配 ----------

// TestImageFileMatches 验证 path.Base 语义：download_url 带目录路径、带查询
// 串时只取尾段文件名；URL 解析失败返回 false。
func TestImageFileMatches(t *testing.T) {
	cases := []struct {
		name, imageURL, filename string
		want                     bool
	}{
		{"basename matches", "https://cloud.example.com/img/debian.qcow2", "debian.qcow2", true},
		{"url with query", "https://cloud.example.com/img/debian.qcow2?version=1", "debian.qcow2", true},
		{"deep path", "https://cloud.example.com/2026/08/noble-server-cloudimg-amd64.img", "noble-server-cloudimg-amd64.img", true},
		{"name mismatch", "https://cloud.example.com/img/debian.qcow2", "ubuntu.qcow2", false},
		{"trailing slash basename", "https://cloud.example.com/img/debian/", "debian", true},
		{"unparsable url", "://nope", "debian.qcow2", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := imageFileMatches(tc.imageURL, tc.filename); got != tc.want {
				t.Fatalf("imageFileMatches(%q, %q) = %v, want %v", tc.imageURL, tc.filename, got, tc.want)
			}
		})
	}
}

// ---------- 节点扫描 ----------

// TestScanImageOnNodes 验证单镜像节点扫描：PVE 名优先、业务名回退（pve2 的
// PveName 为空）、匹配携带 volid、节点扫描失败降级为未下载而非整体失败。
func TestScanImageOnNodes(t *testing.T) {
	srv := newScriptedImagePVE()
	srv.contents["aeolian1"] = []pve.StorageContent{{VolID: "local:import/debian.qcow2", Name: "debian.qcow2"}}
	srv.contentErr["pve3"] = true // 节点 3 扫描失败

	svc := newImageTestService(t, newImageMockPool(t), &fakeImageNodeRepository{}, newFakeImageOperationRepository(), srv)
	img := &model.Image{ID: 1, Name: "debian-12-cloud", DefaultUser: "debian",
		DownloadURL: "https://cloud.example.com/debian.qcow2"}
	got := svc.scanImageOnNodes(context.Background(), imageTestNodes()[:3], img)

	if len(got) != 3 {
		t.Fatalf("got %d statuses, want 3", len(got))
	}
	if !got[0].Downloaded || got[0].VolID != "local:import/debian.qcow2" ||
		got[0].NodeName != "pve1" || got[0].PveName != "aeolian1" {
		t.Fatalf("node1 status = %+v, want downloaded with volid and pve_name aeolian1", got[0])
	}
	if got[1].Downloaded || got[1].VolID != "" || got[1].PveName != "" || got[1].NodeName != "pve2" {
		t.Fatalf("node2 status = %+v, want not downloaded (pve_name empty)", got[1])
	}
	if got[2].Downloaded || got[2].VolID != "" {
		t.Fatalf("node3 status = %+v, want degraded to not downloaded on scan failure", got[2])
	}
}

// ---------- ListImagesByZone ----------

// TestListImagesByZone 验证存在性过滤语义：区域内至少一个启用节点拥有镜像
// 文件才出现在结果中，并携带各启用节点的存在状态；分页在过滤后应用。
func TestListImagesByZone(t *testing.T) {
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

	t.Run("existence filter with per-node statuses", func(t *testing.T) {
		srv := newScriptedImagePVE()
		// debian 只在节点 aeolian1 上；ubuntu 任何节点都没有。
		srv.contents["aeolian1"] = []pve.StorageContent{{VolID: "local:import/debian.qcow2", Name: "debian.qcow2"}}

		mock := newImageMockPool(t)
		expectZone(mock, 1, true)
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images ORDER BY id").
			WillReturnRows(images())
		nodeRepo := &fakeImageNodeRepository{nodes: imageTestNodes()}
		svc := newImageTestService(t, mock, nodeRepo, newFakeImageOperationRepository(), srv)

		items, total, err := svc.ListImagesByZone(context.Background(), 1, 100, 0)
		if err != nil {
			t.Fatalf("ListImagesByZone: %v", err)
		}
		if total != 1 || len(items) != 1 {
			t.Fatalf("got %d items (total %d), want 1", len(items), total)
		}
		item := items[0]
		if item.Image.Name != "debian-12-cloud" || item.Image.ID != 1 {
			t.Fatalf("item = %+v, want debian-12-cloud", item)
		}
		// 区域 1 启用节点按 id 排序：pve1 已下载、pve2 未下载。
		if len(item.Nodes) != 2 {
			t.Fatalf("nodes = %d, want 2", len(item.Nodes))
		}
		if !item.Nodes[0].Downloaded || item.Nodes[0].NodeName != "pve1" || item.Nodes[0].PveName != "aeolian1" {
			t.Fatalf("nodes[0] = %+v, want pve1 downloaded", item.Nodes[0])
		}
		if item.Nodes[1].Downloaded || item.Nodes[1].NodeName != "pve2" {
			t.Fatalf("nodes[1] = %+v, want pve2 not downloaded", item.Nodes[1])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("zone not found", func(t *testing.T) {
		mock := newImageMockPool(t)
		expectZone(mock, 99, false)
		svc := newImageTestService(t, mock, &fakeImageNodeRepository{}, newFakeImageOperationRepository(), newScriptedImagePVE())
		_, _, err := svc.ListImagesByZone(context.Background(), 99, 100, 0)
		assertServiceErrorKind(t, err, KindNotFound)
	})

	t.Run("no enabled nodes yields empty page", func(t *testing.T) {
		mock := newImageMockPool(t)
		expectZone(mock, 3, true) // 区域 3 无节点
		svc := newImageTestService(t, mock, &fakeImageNodeRepository{nodes: imageTestNodes()}, newFakeImageOperationRepository(), newScriptedImagePVE())
		items, total, err := svc.ListImagesByZone(context.Background(), 3, 100, 0)
		if err != nil {
			t.Fatalf("ListImagesByZone: %v", err)
		}
		if total != 0 || len(items) != 0 || items == nil {
			t.Fatalf("got %d items (total %d), want empty non-nil page", len(items), total)
		}
	})

	t.Run("pagination applied after filtering", func(t *testing.T) {
		srv := newScriptedImagePVE()
		srv.contents["aeolian1"] = []pve.StorageContent{{VolID: "local:import/debian.qcow2", Name: "debian.qcow2"}}
		srv.contents["pve2"] = []pve.StorageContent{{VolID: "local:import/ubuntu.qcow2", Name: "ubuntu.qcow2"}}

		mock := newImageMockPool(t)
		expectZone(mock, 1, true)
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images ORDER BY id").
			WillReturnRows(images())
		svc := newImageTestService(t, mock, &fakeImageNodeRepository{nodes: imageTestNodes()}, newFakeImageOperationRepository(), srv)

		items, total, err := svc.ListImagesByZone(context.Background(), 1, 1, 1)
		if err != nil {
			t.Fatalf("ListImagesByZone: %v", err)
		}
		if total != 2 || len(items) != 1 {
			t.Fatalf("got %d items (total %d), want 1 of 2", len(items), total)
		}
		if items[0].Image.Name != "ubuntu-24-cloud" {
			t.Fatalf("item = %+v, want ubuntu-24-cloud (second page)", items[0])
		}
	})

	t.Run("node scan failure excludes image", func(t *testing.T) {
		srv := newScriptedImagePVE()
		// 唯一的启用节点扫描失败：镜像全部降级为不存在。
		srv.contentErr["aeolian1"] = true

		mock := newImageMockPool(t)
		expectZone(mock, 1, true)
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images ORDER BY id").
			WillReturnRows(images())
		svc := newImageTestService(t, mock, &fakeImageNodeRepository{nodes: imageTestNodes()}, newFakeImageOperationRepository(), srv)

		items, total, err := svc.ListImagesByZone(context.Background(), 1, 100, 0)
		if err != nil {
			t.Fatalf("ListImagesByZone: %v", err)
		}
		if total != 0 || len(items) != 0 {
			t.Fatalf("got %d items (total %d), want empty", len(items), total)
		}
	})
}

// ---------- Download ----------

// TestDownload validates 校验：image_id 正数、镜像存在、download_url 协议。
func TestDownloadValidates(t *testing.T) {
	const debianURL = "https://cloud.example.com/debian.qcow2"

	t.Run("image id must be positive", func(t *testing.T) {
		svc := newImageTestService(t, newImageMockPool(t), &fakeImageNodeRepository{}, newFakeImageOperationRepository(), newScriptedImagePVE())
		_, err := svc.Download(context.Background(), 0, []int64{1}, nil)
		assertServiceErrorKind(t, err, KindBadRequest)
	})

	t.Run("zone id and node ids mutually exclusive", func(t *testing.T) {
		svc := newImageTestService(t, newImageMockPool(t), &fakeImageNodeRepository{}, newFakeImageOperationRepository(), newScriptedImagePVE())
		zoneID := int64(1)
		_, err := svc.Download(context.Background(), 1, []int64{1}, &zoneID)
		assertServiceErrorKind(t, err, KindBadRequest)
	})

	t.Run("image not found", func(t *testing.T) {
		mock := newImageMockPool(t)
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images WHERE id=\\$1").
			WithArgs(int64(99)).WillReturnError(pgx.ErrNoRows)
		svc := newImageTestService(t, mock, &fakeImageNodeRepository{}, newFakeImageOperationRepository(), newScriptedImagePVE())
		_, err := svc.Download(context.Background(), 99, []int64{1}, nil)
		assertServiceErrorKind(t, err, KindNotFound)
	})

	t.Run("non-http download url rejected", func(t *testing.T) {
		mock := newImageMockPool(t)
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images WHERE id=\\$1").
			WithArgs(int64(1)).
			WillReturnRows(pgxmock.NewRows(imageRowCols).
				AddRow(int64(1), "debian-12-cloud", "debian", "file:///etc/passwd", imageTestTime))
		svc := newImageTestService(t, mock, &fakeImageNodeRepository{}, newFakeImageOperationRepository(), newScriptedImagePVE())
		_, err := svc.Download(context.Background(), 1, []int64{1}, nil)
		assertServiceErrorKind(t, err, KindBadRequest)
	})
}

// TestDownloadResolvesNodes 验证目标节点解析：nodeIDs 逐个校验（不存在报
// not_found）、重复 id 按出现顺序去重、空列表报 bad_request、node_ids 超过
// 批量上限报 bad_request、zone 模式取区域启用节点（区域不存在报 not_found）。
func TestDownloadResolvesNodes(t *testing.T) {
	const debianURL = "https://cloud.debian.org/debian.qcow2"
	expectImage := func(mock pgxmock.PgxPoolIface) {
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images WHERE id=\\$1").
			WithArgs(int64(1)).
			WillReturnRows(pgxmock.NewRows(imageRowCols).
				AddRow(int64(1), "debian-12-cloud", "debian", debianURL, imageTestTime))
	}
	expectZone := func(mock pgxmock.PgxPoolIface, zoneID int64, exists bool) {
		mock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM zones WHERE id=\\$1\\)").
			WithArgs(zoneID).
			WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(exists))
	}

	t.Run("node ids mode with unknown node", func(t *testing.T) {
		mock := newImageMockPool(t)
		expectImage(mock)
		nodeRepo := &fakeImageNodeRepository{nodes: imageTestNodes()}
		svc := newImageTestService(t, mock, nodeRepo, newFakeImageOperationRepository(), newScriptedImagePVE())
		_, err := svc.Download(context.Background(), 1, []int64{1, 99}, nil)
		assertServiceErrorKind(t, err, KindNotFound)
	})

	t.Run("node ids mode deduplicates repeated ids", func(t *testing.T) {
		mock := newImageMockPool(t)
		expectImage(mock)
		nodeRepo := &fakeImageNodeRepository{nodes: imageTestNodes()}
		opRepo := newFakeImageOperationRepository()
		svc := newImageTestService(t, mock, nodeRepo, opRepo, newScriptedImagePVE())
		// [1,1] 只建一条 running 记录，绝不并发下载同一节点两次。
		ops, err := svc.Download(context.Background(), 1, []int64{1, 1}, nil)
		if err != nil {
			t.Fatalf("Download: %v", err)
		}
		if len(ops) != 1 || ops[0].NodeID != 1 {
			t.Fatalf("ops = %+v, want single running op for node 1", ops)
		}
	})

	t.Run("empty node list rejected", func(t *testing.T) {
		mock := newImageMockPool(t)
		expectImage(mock)
		svc := newImageTestService(t, mock, &fakeImageNodeRepository{nodes: imageTestNodes()}, newFakeImageOperationRepository(), newScriptedImagePVE())
		_, err := svc.Download(context.Background(), 1, nil, nil)
		assertServiceErrorKind(t, err, KindBadRequest)
	})

	t.Run("too many node ids rejected", func(t *testing.T) {
		mock := newImageMockPool(t)
		expectImage(mock)
		// 超过 maxImageDownloadNodes（64）的 node_ids 在解析前就被拒绝。
		ids := make([]int64, maxImageDownloadNodes+1)
		for i := range ids {
			ids[i] = int64(i + 1)
		}
		svc := newImageTestService(t, mock, &fakeImageNodeRepository{nodes: imageTestNodes()}, newFakeImageOperationRepository(), newScriptedImagePVE())
		_, err := svc.Download(context.Background(), 1, ids, nil)
		assertServiceErrorKind(t, err, KindBadRequest)
	})

	t.Run("zone mode with unknown zone", func(t *testing.T) {
		mock := newImageMockPool(t)
		expectImage(mock)
		expectZone(mock, 99, false)
		svc := newImageTestService(t, mock, &fakeImageNodeRepository{nodes: imageTestNodes()}, newFakeImageOperationRepository(), newScriptedImagePVE())
		zoneID := int64(99)
		_, err := svc.Download(context.Background(), 1, nil, &zoneID)
		assertServiceErrorKind(t, err, KindNotFound)
	})

	t.Run("zone mode resolves enabled nodes", func(t *testing.T) {
		mock := newImageMockPool(t)
		expectImage(mock)
		expectZone(mock, 1, true)
		nodeRepo := &fakeImageNodeRepository{nodes: imageTestNodes()}
		opRepo := newFakeImageOperationRepository()
		svc := newImageTestService(t, mock, nodeRepo, opRepo, newScriptedImagePVE())
		zoneID := int64(1)
		ops, err := svc.Download(context.Background(), 1, nil, &zoneID)
		if err != nil {
			t.Fatalf("Download: %v", err)
		}
		// 区域 1 的启用节点：pve1、pve2，各一条 running 记录。
		if len(ops) != 2 {
			t.Fatalf("got %d operations, want 2", len(ops))
		}
		for _, op := range ops {
			if op.ImageID != 1 || op.Action != model.ImageOpActionDownload || op.Result != model.ImageOpResultRunning {
				t.Fatalf("op = %+v, want download/running for image 1", op)
			}
		}
		if ops[0].NodeID != 1 || ops[1].NodeID != 2 {
			t.Fatalf("op node ids = %d/%d, want 1/2", ops[0].NodeID, ops[1].NodeID)
		}
	})
}

// TestDownloadRejectsRunningDuplicate 验证幂等：镜像在目标节点上已有
// running 的下载操作时，重复受理被 conflict（handler 映射 409）拒绝
//（消息指出节点）。
func TestDownloadRejectsRunningDuplicate(t *testing.T) {
	const debianURL = "https://cloud.debian.org/debian.qcow2"
	mock := newImageMockPool(t)
	mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images WHERE id=\\$1").
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows(imageRowCols).
			AddRow(int64(1), "debian-12-cloud", "debian", debianURL, imageTestTime))
	nodeRepo := &fakeImageNodeRepository{nodes: imageTestNodes()}
	opRepo := newFakeImageOperationRepository()
	// 预置一条该镜像在节点 1 上未终态的下载记录。
	opRepo.ops = []model.ImageOperation{
		{ID: 9, ImageID: 1, NodeID: 1, Action: model.ImageOpActionDownload, Result: model.ImageOpResultRunning},
	}
	svc := newImageTestService(t, mock, nodeRepo, opRepo, newScriptedImagePVE())

	_, err := svc.Download(context.Background(), 1, []int64{1}, nil)
	assertServiceErrorKind(t, err, KindConflict)
	var se *Error
	errors.As(err, &se)
	if !strings.Contains(se.Message, "already being downloaded") || !strings.Contains(se.Message, "node 1") {
		t.Fatalf("err message = %q, want to name the running node", se.Message)
	}
	if got := opRepo.snapshot(); len(got) != 1 {
		t.Fatalf("ops = %+v, want only the pre-seeded running record (no new operation created)", got)
	}
}

// TestDownloadOrchestrates 验证下载编排的终态流转：running 落库 → 成功
// success+upid；受理失败 failed+error_message（upid 空）；任务失败
// failed+error_message（upid 保留）；panic 被恢复并记录 failed。
func TestDownloadOrchestrates(t *testing.T) {
	const debianURL = "https://cloud.debian.org/debian.qcow2"
	expectImage := func(mock pgxmock.PgxPoolIface) {
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images WHERE id=\\$1").
			WithArgs(int64(1)).
			WillReturnRows(pgxmock.NewRows(imageRowCols).
				AddRow(int64(1), "debian-12-cloud", "debian", debianURL, imageTestTime))
	}

	t.Run("success on both nodes", func(t *testing.T) {
		srv := newScriptedImagePVE()
		mock := newImageMockPool(t)
		expectImage(mock)
		opRepo := newFakeImageOperationRepository()
		svc := newImageTestService(t, mock, &fakeImageNodeRepository{nodes: imageTestNodes()}, opRepo, srv)

		ops, err := svc.Download(context.Background(), 1, []int64{1, 2}, nil)
		if err != nil {
			t.Fatalf("Download: %v", err)
		}
		if len(ops) != 2 {
			t.Fatalf("got %d operations, want 2", len(ops))
		}
		waitForImageDownloads(t, opRepo)

		snapshot := opRepo.snapshot()
		for _, op := range snapshot {
			if op.Result != model.ImageOpResultSuccess || op.ErrorMessage != "" || op.UPID == "" {
				t.Fatalf("op = %+v, want success with upid and no error", op)
			}
		}
		// 受理按节点 PVE 名分发：pve1 -> aeolian1，pve2 -> pve2；两个 goroutine
		// 并发执行，顺序无保证，按集合断言。
		got := srv.receivedDownloads()
		if len(got) != 2 ||
			!(got[0] == "aeolian1" && got[1] == "pve2") && !(got[0] == "pve2" && got[1] == "aeolian1") {
			t.Fatalf("downloads = %v, want [aeolian1 pve2] in any order", got)
		}
	})

	t.Run("download-url rejection recorded as failed", func(t *testing.T) {
		srv := newScriptedImagePVE()
		srv.downloadErr["aeolian1"] = true
		mock := newImageMockPool(t)
		expectImage(mock)
		opRepo := newFakeImageOperationRepository()
		svc := newImageTestService(t, mock, &fakeImageNodeRepository{nodes: imageTestNodes()}, opRepo, srv)

		if _, err := svc.Download(context.Background(), 1, []int64{1}, nil); err != nil {
			t.Fatalf("Download: %v", err)
		}
		waitForImageDownloads(t, opRepo)

		op := opRepo.snapshot()[0]
		if op.Result != model.ImageOpResultFailed || op.UPID != "" || !strings.Contains(op.ErrorMessage, "download-url rejected") {
			t.Fatalf("op = %+v, want failed with message and empty upid", op)
		}
	})

	t.Run("task failure recorded as failed", func(t *testing.T) {
		srv := newScriptedImagePVE()
		srv.taskErr["UPID:aeolian1:00000001:00000002:5FAB1EC4:imgcopy:1:root@pam:"] = true
		mock := newImageMockPool(t)
		expectImage(mock)
		opRepo := newFakeImageOperationRepository()
		svc := newImageTestService(t, mock, &fakeImageNodeRepository{nodes: imageTestNodes()}, opRepo, srv)

		if _, err := svc.Download(context.Background(), 1, []int64{1}, nil); err != nil {
			t.Fatalf("Download: %v", err)
		}
		waitForImageDownloads(t, opRepo)

		op := opRepo.snapshot()[0]
		if op.Result != model.ImageOpResultFailed || !strings.Contains(op.ErrorMessage, "task status boom") || op.UPID == "" {
			t.Fatalf("op = %+v, want failed with task error and upid preserved", op)
		}
	})

	t.Run("panic recovered and recorded as failed", func(t *testing.T) {
		srv := newScriptedImagePVE()
		mock := newImageMockPool(t)
		expectImage(mock)
		opRepo := newFakeImageOperationRepository()
		svc := newImageTestService(t, mock, &fakeImageNodeRepository{nodes: imageTestNodes()}, opRepo, srv)

		// 覆盖客户端工厂：download-url 请求在传输层 panic，验证 goroutine 的
		// panic 恢复路径（绝不会拖垮进程）。
		ts := httptest.NewServer(srv.handler())
		t.Cleanup(ts.Close)
		panicTransport := &panicRoundTripper{base: ts.Client().Transport}
		svc.SetClientFactory(func(host string, port int, apiUser, apiTokenSecret string) *pve.Client {
			return pve.NewClient(host, apiUser, apiTokenSecret,
				pve.WithBaseURL(ts.URL), pve.WithHTTPClient(&http.Client{Transport: panicTransport}), pve.WithTimeout(5*time.Second))
		})

		if _, err := svc.Download(context.Background(), 1, []int64{1}, nil); err != nil {
			t.Fatalf("Download: %v", err)
		}
		waitForImageDownloads(t, opRepo)

		op := opRepo.snapshot()[0]
		if op.Result != model.ImageOpResultFailed || !strings.Contains(op.ErrorMessage, "internal panic") ||
			!strings.Contains(op.ErrorMessage, "fake panic") {
			t.Fatalf("op = %+v, want failed with internal panic message", op)
		}
	})
}

// panicRoundTripper 在 download-url 请求上 panic（其余请求透传），用于验证
// 下载 goroutine 的 panic 恢复路径。
type panicRoundTripper struct {
	base http.RoundTripper
}

func (p *panicRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Path, "download-url") {
		panic("fake panic in pve client")
	}
	return p.base.RoundTrip(req)
}

// ---------- 错误消息脱敏（L2） ----------

// TestSanitizeImageOperationErrorRedactsDownloadURL 验证落库错误消息中的
// download_url 原文（可能携带签名等敏感查询参数）被替换为 [redacted]：
// 覆盖 PVE UpstreamError（errors 对象原样保留消息）与普通错误两种来源。
func TestSanitizeImageOperationErrorRedactsDownloadURL(t *testing.T) {
	const url = "https://cloud.debian.org/cloud/img.qcow2?token=secret"

	t.Run("upstream error carries the url", func(t *testing.T) {
		err := &pve.UpstreamError{Errors: map[string]string{"url": "cannot download: " + url}}
		msg := sanitizeImageOperationError(err, url)
		if strings.Contains(msg, url) || !strings.Contains(msg, "[redacted]") {
			t.Fatalf("msg = %q, want url redacted", msg)
		}
		if !strings.Contains(msg, "cannot download") {
			t.Fatalf("msg = %q, want to keep the sanitized reason", msg)
		}
	})

	t.Run("plain error carries the url", func(t *testing.T) {
		msg := sanitizeImageOperationError(errors.New("download failed for "+url), url)
		if strings.Contains(msg, url) || !strings.Contains(msg, "[redacted]") {
			t.Fatalf("msg = %q, want url redacted", msg)
		}
	})

	t.Run("empty download url leaves message untouched", func(t *testing.T) {
		const m = "plain failure"
		if got := sanitizeImageOperationError(errors.New(m), ""); got != m {
			t.Fatalf("msg = %q, want %q", got, m)
		}
	})
}

// TestFinishDownloadRedactsDownloadURL 验证脱敏发生在完整落库链路
//（finishDownload 拿到 download_url 参数）而非仅辅助函数内：落库的
// error_message 不含 URL 原文。
func TestFinishDownloadRedactsDownloadURL(t *testing.T) {
	const url = "https://cloud.debian.org/cloud/img.qcow2?token=secret"
	opRepo := newFakeImageOperationRepository()
	opRepo.ops = []model.ImageOperation{
		{ID: 1, ImageID: 1, NodeID: 1, Action: model.ImageOpActionDownload, Result: model.ImageOpResultRunning},
	}
	svc := NewImageService(repository.NewImageRepository(newImageMockPool(t)), &fakeImageNodeRepository{}, opRepo)
	svc.finishDownload(context.Background(), 1, model.ImageOpResultFailed,
		errors.New("download failed for "+url), "", url)

	op := opRepo.snapshot()[0]
	if op.Result != model.ImageOpResultFailed || strings.Contains(op.ErrorMessage, url) ||
		!strings.Contains(op.ErrorMessage, "[redacted]") {
		t.Fatalf("op = %+v, want failed with url redacted", op)
	}
}

// ---------- GetImageNodeStatus ----------

// TestGetImageNodeStatus 验证单镜像节点状态：镜像必须存在；zoneID 非空只扫
// 区域启用节点，为空扫全部启用节点；结果按节点 id 排序。
func TestGetImageNodeStatus(t *testing.T) {
	const (
		debianURL = "https://cloud.example.com/debian.qcow2"
		ubuntuURL = "https://cloud.example.com/ubuntu.qcow2"
	)
	expectImage := func(mock pgxmock.PgxPoolIface) {
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images WHERE id=\\$1").
			WithArgs(int64(1)).
			WillReturnRows(pgxmock.NewRows(imageRowCols).
				AddRow(int64(1), "debian-12-cloud", "debian", debianURL, imageTestTime))
	}
	expectZone := func(mock pgxmock.PgxPoolIface, zoneID int64, exists bool) {
		mock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM zones WHERE id=\\$1\\)").
			WithArgs(zoneID).
			WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(exists))
	}

	t.Run("image not found", func(t *testing.T) {
		mock := newImageMockPool(t)
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images WHERE id=\\$1").
			WithArgs(int64(99)).WillReturnError(pgx.ErrNoRows)
		svc := newImageTestService(t, mock, &fakeImageNodeRepository{}, newFakeImageOperationRepository(), newScriptedImagePVE())
		_, err := svc.GetImageNodeStatus(context.Background(), 99, nil)
		assertServiceErrorKind(t, err, KindNotFound)
	})

	t.Run("zone not found", func(t *testing.T) {
		mock := newImageMockPool(t)
		expectImage(mock)
		expectZone(mock, 99, false)
		svc := newImageTestService(t, mock, &fakeImageNodeRepository{nodes: imageTestNodes()}, newFakeImageOperationRepository(), newScriptedImagePVE())
		zoneID := int64(99)
		_, err := svc.GetImageNodeStatus(context.Background(), 1, &zoneID)
		assertServiceErrorKind(t, err, KindNotFound)
	})

	t.Run("zone scoped", func(t *testing.T) {
		srv := newScriptedImagePVE()
		srv.contents["aeolian1"] = []pve.StorageContent{{VolID: "local:import/debian.qcow2", Name: "debian.qcow2"}}
		mock := newImageMockPool(t)
		expectImage(mock)
		expectZone(mock, 1, true)
		svc := newImageTestService(t, mock, &fakeImageNodeRepository{nodes: imageTestNodes()}, newFakeImageOperationRepository(), srv)

		zoneID := int64(1)
		got, err := svc.GetImageNodeStatus(context.Background(), 1, &zoneID)
		if err != nil {
			t.Fatalf("GetImageNodeStatus: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d statuses, want 2 (zone 1 enabled nodes)", len(got))
		}
		if got[0].NodeID != 1 || !got[0].Downloaded || got[1].NodeID != 2 || got[1].Downloaded {
			t.Fatalf("statuses = %+v, want pve1 downloaded / pve2 not", got)
		}
	})

	t.Run("all enabled nodes when zone absent", func(t *testing.T) {
		srv := newScriptedImagePVE()
		srv.contents["aeolian4"] = []pve.StorageContent{{VolID: "local:import/ubuntu.qcow2", Name: "ubuntu.qcow2"}}
		mock := newImageMockPool(t)
		expectImage(mock)
		svc := newImageTestService(t, mock, &fakeImageNodeRepository{nodes: imageTestNodes()}, newFakeImageOperationRepository(), srv)

		got, err := svc.GetImageNodeStatus(context.Background(), 1, nil)
		if err != nil {
			t.Fatalf("GetImageNodeStatus: %v", err)
		}
		// 全部启用节点：pve1、pve2、pve4（跨区域，按 id 排序）。
		if len(got) != 3 || got[0].NodeID != 1 || got[1].NodeID != 2 || got[2].NodeID != 4 {
			t.Fatalf("statuses = %+v, want nodes 1/2/4 in order", got)
		}
		if got[2].Downloaded || got[2].VolID != "" {
			t.Fatalf("statuses[2] = %+v, want debian not on pve4 (only ubuntu there)", got[2])
		}
	})
}

// ---------- ListImageOperations ----------

// TestListImageOperations 验证下载历史分页透传：镜像必须存在，limit/offset
// 生效，total 为匹配总数。
func TestListImageOperations(t *testing.T) {
	t.Run("image not found", func(t *testing.T) {
		mock := newImageMockPool(t)
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images WHERE id=\\$1").
			WithArgs(int64(99)).WillReturnError(pgx.ErrNoRows)
		svc := newImageTestService(t, mock, &fakeImageNodeRepository{}, newFakeImageOperationRepository(), newScriptedImagePVE())
		_, _, err := svc.ListImageOperations(context.Background(), 99, 10, 0)
		assertServiceErrorKind(t, err, KindNotFound)
	})

	t.Run("paged listing", func(t *testing.T) {
		mock := newImageMockPool(t)
		mock.ExpectQuery("SELECT id, name, default_user, download_url, created_at FROM images WHERE id=\\$1").
			WithArgs(int64(1)).
			WillReturnRows(pgxmock.NewRows(imageRowCols).
				AddRow(int64(1), "debian-12-cloud", "debian", "https://example.com/d.img", imageTestTime))
		opRepo := newFakeImageOperationRepository()
		opRepo.ops = []model.ImageOperation{
			{ID: 1, ImageID: 1, NodeID: 1, Action: model.ImageOpActionDownload, Result: model.ImageOpResultSuccess},
			{ID: 2, ImageID: 1, NodeID: 2, Action: model.ImageOpActionDownload, Result: model.ImageOpResultFailed},
			{ID: 3, ImageID: 2, NodeID: 1, Action: model.ImageOpActionDownload, Result: model.ImageOpResultRunning},
		}
		svc := newImageTestService(t, mock, &fakeImageNodeRepository{}, opRepo, newScriptedImagePVE())

		ops, total, err := svc.ListImageOperations(context.Background(), 1, 1, 0)
		if err != nil {
			t.Fatalf("ListImageOperations: %v", err)
		}
		if total != 2 || len(ops) != 1 {
			t.Fatalf("got %d ops (total %d), want 1 of 2", len(ops), total)
		}
		// 倒序：最新的 id=2 在前。
		if ops[0].ID != 2 {
			t.Fatalf("op = %+v, want id 2 first (newest)", ops[0])
		}
	})
}

// TestSlicePage 固定区域镜像列表使用的共享 Go 侧页切片行为：切片绝不会越出
// 末尾，越界的 offset 产生空结果，limit 为 0 产生空页。负的 limit/offset 会
// 被钳制为 0（HTTP 层会拒绝它们，但共享辅助函数不能因调用方传入负值而 panic
// 或错误切片）。
func TestSlicePage(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}
	cases := []struct {
		name       string
		limit, off int
		want       []int
	}{
		{"first page", 2, 0, []int{1, 2}},
		{"middle page", 2, 2, []int{3, 4}},
		{"last short page", 2, 4, []int{5}},
		{"offset past the end", 2, 10, []int{}},
		{"limit 0", 0, 0, []int{}},
		{"exact end", 5, 0, []int{1, 2, 3, 4, 5}},
		{"negative limit clamps to 0", -1, 0, []int{}},
		{"negative offset clamps to 0", 2, -3, []int{1, 2}},
		{"both negative", -1, -1, []int{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := slicePage(items, tc.limit, tc.off)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
