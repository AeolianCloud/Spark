package pve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"testing"
)

// TestListStorage 验证集群级 GET /storage 的请求形态（无 path 参数、
// 无查询参数）以及响应条目的字段映射；真实 PVE 的 nodes 为逗号分隔
// 字符串（如 "pve1,pve2"），content 可能为空串。
func TestListStorage(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/storage" {
			t.Errorf("path = %s, want /storage", r.URL.Path)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("query = %q, want none", r.URL.RawQuery)
		}
		fmt.Fprint(w, `{"data": [
			{"storage": "local", "type": "dir", "content": "images,iso,backup", "shared": 0, "nodes": "pve1,pve2"},
			{"storage": "local-lvm", "type": "lvm", "content": "images,rootdir", "shared": 1, "nodes": "pve1"},
			{"storage": "nfs-iso", "type": "nfs", "content": "", "shared": 1, "nodes": "pve1,pve2,pve3"}
		]}`)
	})
	storages, err := c.ListStorage(context.Background())
	if err != nil {
		t.Fatalf("ListStorage: %v", err)
	}
	if len(storages) != 3 {
		t.Fatalf("len = %d, want 3", len(storages))
	}
	if a := storages[0]; a.Storage != "local" || a.Type != "dir" || a.Content != "images,iso,backup" ||
		bool(a.Shared) || len(a.Nodes) != 2 || a.Nodes[0] != "pve1" || a.Nodes[1] != "pve2" {
		t.Fatalf("storages[0] = %+v", a)
	}
	if b := storages[1]; b.Storage != "local-lvm" || b.Type != "lvm" || !bool(b.Shared) {
		t.Fatalf("storages[1] = %+v", b)
	}
	// content 空串（PVE 真实形态）原样保留，不推导。
	if d := storages[2]; d.Content != "" || len(d.Nodes) != 3 {
		t.Fatalf("storages[2] = %+v, want empty content and 3 nodes", d)
	}
}

// TestListStorageNodesArray 覆盖 nodes 字段的数组形态（部分 PVE 场景），
// 以及 nodes 缺失/为 null 时保持空切片、不报错。
func TestListStorageNodesArray(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": [
			{"storage": "local", "type": "dir", "content": "images", "shared": 0, "nodes": ["pve1", "pve2"]},
			{"storage": "snippets", "type": "dir", "content": "snippets", "shared": 0}
		]}`)
	})
	storages, err := c.ListStorage(context.Background())
	if err != nil {
		t.Fatalf("ListStorage: %v", err)
	}
	if len(storages) != 2 {
		t.Fatalf("len = %d, want 2", len(storages))
	}
	if a := storages[0]; len(a.Nodes) != 2 || a.Nodes[0] != "pve1" || a.Nodes[1] != "pve2" {
		t.Fatalf("storages[0] nodes = %+v, want [pve1 pve2] from array form", a.Nodes)
	}
	if b := storages[1]; len(b.Nodes) != 0 {
		t.Fatalf("storages[1] nodes = %+v, want empty when the field is absent", b.Nodes)
	}
}

// TestListStorageUpstreamError 覆盖 PVE 拒绝（如 token 无权限）时以
// *UpstreamError 呈现的场景。
func TestListStorageUpstreamError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"errors": {"root@pam": "permission denied"}}`)
	})
	_, err := c.ListStorage(context.Background())
	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("err = %v (%T), want *UpstreamError", err, err)
	}
	if upErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("StatusCode = %d, want 401", upErr.StatusCode)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error %q does not carry the PVE error", err.Error())
	}
}

// TestListStorageContent 验证 GET /nodes/{node}/storage/{storage}/content
// 的请求形态（路径与 content 查询参数）以及响应条目的字段映射。
func TestListStorageContent(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/nodes/pve1/storage/local/content" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("content"); got != "import" {
			t.Errorf("content = %q, want import", got)
		}
		fmt.Fprint(w, `{"data": [
			{"volid": "local:import/debian-12-genericcloud-amd64.qcow2", "name": "debian-12-genericcloud-amd64.qcow2"},
			{"volid": "local:import/ubuntu-22.04-cloudimg.qcow2", "name": "ubuntu-22.04-cloudimg.qcow2"}
		]}`)
	})
	items, err := c.ListStorageContent(context.Background(), "pve1", "local", "import")
	if err != nil {
		t.Fatalf("ListStorageContent: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len = %d, want 2", len(items))
	}
	if a := items[0]; a.VolID != "local:import/debian-12-genericcloud-amd64.qcow2" ||
		a.Name != "debian-12-genericcloud-amd64.qcow2" {
		t.Fatalf("items[0] = %+v", a)
	}
	if b := items[1]; b.VolID != "local:import/ubuntu-22.04-cloudimg.qcow2" ||
		b.Name != "ubuntu-22.04-cloudimg.qcow2" {
		t.Fatalf("items[1] = %+v", b)
	}
}

// TestListStorageContentWithoutName 覆盖真实 PVE 的响应形态：条目只有
// volid 而没有 name 字段（已实测），此时 Name 应从 volid 推导（取最后
// 一个 "/" 之后的段）。
func TestListStorageContentWithoutName(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": [
			{"volid": "local:import/debian-12-genericcloud-amd64.qcow2", "content": "import", "format": "qcow2", "size": 2147483648},
			{"volid": "local:import/ubuntu-22.04-cloudimg.qcow2"},
			{"volid": "nodir"}
		]}`)
	})
	items, err := c.ListStorageContent(context.Background(), "pve1", "local", "import")
	if err != nil {
		t.Fatalf("ListStorageContent: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("len = %d, want 3", len(items))
	}
	if items[0].Name != "debian-12-genericcloud-amd64.qcow2" {
		t.Fatalf("items[0].Name = %q, want derived from volid", items[0].Name)
	}
	if items[1].Name != "ubuntu-22.04-cloudimg.qcow2" {
		t.Fatalf("items[1].Name = %q, want derived from volid", items[1].Name)
	}
	// volid 解析不出文件名时 Name 保持空串，不 panic，上层照旧不匹配。
	if items[2].Name != "" {
		t.Fatalf("items[2].Name = %q, want empty", items[2].Name)
	}
}

// TestListStorageContentNamePrecedence 验证响应携带 name 字段时以响应值
// 为准，不因 volid 推导而覆盖。
func TestListStorageContentNamePrecedence(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": [
			{"volid": "local:import/disk-a.qcow2", "name": "custom-name.qcow2"}
		]}`)
	})
	items, err := c.ListStorageContent(context.Background(), "pve1", "local", "import")
	if err != nil {
		t.Fatalf("ListStorageContent: %v", err)
	}
	if len(items) != 1 || items[0].Name != "custom-name.qcow2" {
		t.Fatalf("items = %+v, want name from response", items)
	}
}

// TestListStorageContentUpstreamError 覆盖 PVE 拒绝（如存储不存在）时以
// *UpstreamError 呈现的场景。
func TestListStorageContentUpstreamError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"errors": {"storage": "storage 'nope' does not exist"}}`)
	})
	_, err := c.ListStorageContent(context.Background(), "pve1", "nope", "import")
	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("err = %v (%T), want *UpstreamError", err, err)
	}
	if upErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("StatusCode = %d, want 400", upErr.StatusCode)
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("error %q does not carry the PVE error", err.Error())
	}
}

// TestDownloadURL 验证 POST /nodes/{node}/storage/{storage}/download-url
// 以 form-urlencoded 发送 content/filename/url 参数（URL 中的特殊字符
// 必须被编码），并解析返回的 UPID。
func TestDownloadURL(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/nodes/pve1/storage/local/download-url" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", ct)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		// URL 中的 ":" 与 "/" 等特殊字符必须被 form 编码。
		if !strings.Contains(string(body), "url=https%3A%2F%2Fcloud.example.com%2Fimages%2Fdebian-12.qcow2") {
			t.Errorf("body %q does not carry the form-encoded URL", body)
		}
		form, err := neturl.ParseQuery(string(body))
		if err != nil {
			t.Errorf("parse query: %v", err)
			return
		}
		if form.Get("content") != "import" ||
			form.Get("filename") != "debian-12-genericcloud-amd64.qcow2" ||
			form.Get("url") != "https://cloud.example.com/images/debian-12.qcow2" {
			t.Errorf("form = %v", form)
		}
		fmt.Fprint(w, `{"data": "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:vzdldownload:100:root@pam:"}`)
	})
	upid, err := c.DownloadURL(context.Background(), "pve1", "local", "import",
		"debian-12-genericcloud-amd64.qcow2", "https://cloud.example.com/images/debian-12.qcow2")
	if err != nil {
		t.Fatalf("DownloadURL: %v", err)
	}
	if !strings.HasPrefix(upid, "UPID:") {
		t.Fatalf("upid = %q, want a UPID", upid)
	}
}

// TestDownloadURLWithoutContent 验证 content 传空串时不发送 content
// form 参数（与 ListStorageContent 的空值策略一致）。
func TestDownloadURLWithoutContent(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		form, err := neturl.ParseQuery(string(body))
		if err != nil {
			t.Errorf("parse query: %v", err)
			return
		}
		if _, ok := form["content"]; ok {
			t.Errorf("form carries content=%q, want parameter omitted", form.Get("content"))
		}
		if form.Get("filename") != "debian-12-genericcloud-amd64.qcow2" ||
			form.Get("url") != "https://cloud.example.com/images/debian-12.qcow2" {
			t.Errorf("form = %v", form)
		}
		fmt.Fprint(w, `{"data": "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:vzdldownload:100:root@pam:"}`)
	})
	upid, err := c.DownloadURL(context.Background(), "pve1", "local", "",
		"debian-12-genericcloud-amd64.qcow2", "https://cloud.example.com/images/debian-12.qcow2")
	if err != nil {
		t.Fatalf("DownloadURL: %v", err)
	}
	if !strings.HasPrefix(upid, "UPID:") {
		t.Fatalf("upid = %q, want a UPID", upid)
	}
}

// TestDownloadURLEmptyURL 验证 url 为空串时直接返回错误，不发起请求。
func TestDownloadURLEmptyURL(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})
	_, err := c.DownloadURL(context.Background(), "pve1", "local", "import",
		"debian-12-genericcloud-amd64.qcow2", "")
	if err == nil {
		t.Fatal("DownloadURL with empty url: want error, got nil")
	}
	if !strings.Contains(err.Error(), "empty url") {
		t.Fatalf("err = %q, want empty url error", err.Error())
	}
}

// TestDownloadURLUpstreamError 覆盖下载被 PVE 拒绝（如 URL 不可达、文件
// 已存在）时以 *UpstreamError 呈现的场景。
func TestDownloadURLUpstreamError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"errors": {"url": "unable to get 'https://cloud.example.com/images/debian-12.qcow2' - HTTP ERROR 404"}}`)
	})
	_, err := c.DownloadURL(context.Background(), "pve1", "local", "import",
		"debian-12-genericcloud-amd64.qcow2", "https://cloud.example.com/images/debian-12.qcow2")
	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("err = %v (%T), want *UpstreamError", err, err)
	}
	if upErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("StatusCode = %d, want 400", upErr.StatusCode)
	}
	if !strings.Contains(err.Error(), "HTTP ERROR 404") {
		t.Fatalf("error %q does not carry the PVE error", err.Error())
	}
}
