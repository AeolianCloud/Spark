package pve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
)

// StorageContent 是 GET /nodes/{node}/storage/{storage}/content 返回的
// 单个条目（如 import 目录下的云镜像文件）。该列表接口只依赖 VolID 与
// Name 两个字段，其余字段（size、ctime 等）未建模。
type StorageContent struct {
	// VolID 是卷 ID，如 local:import/debian-12-genericcloud-amd64.qcow2。
	VolID string `json:"volid"`
	// Name 是文件名，如 debian-12-genericcloud-amd64.qcow2。真实 PVE 响应
	// 不提供该字段（已实测），ListStorageContent 会从 VolID 推导文件名
	// （取最后一个 "/" 之后的段）；响应携带该字段时以响应值为准。
	Name string `json:"name"`
}

// PVEStorage 是 GET /storage 返回的单个集群存储（cfs 配置条目）。
// 集群级接口，任一节点凭据均可调用，各节点看到同一份配置。
type PVEStorage struct {
	// Storage 是 PVE 存储名，在集群内唯一（如 local、local-lvm）。
	Storage string `json:"storage"`
	// Type 是存储类型（dir/lvm/zfspool/nfs/cifs 等）。
	Type string `json:"type"`
	// Content 是内容能力声明，逗号分隔（如 "images,iso"）；空串表示该
	// 存储未声明任何内容类型。
	Content string `json:"content"`
	// Shared 指示该存储是否为集群共享存储。PVE 返回数字 1/0 或布尔
	// true/false 两种形态，用 PveBool 双格式兼容。
	Shared PveBool `json:"shared"`
	// Nodes 是使用该存储的节点名列表。真实 PVE 返回逗号分隔字符串
	// （如 "pve1,pve2"，已实测），部分场景可能返回数组，两种形式都兼容。
	Nodes []string `json:"nodes"`
}

// UnmarshalJSON 兼容 PVE 对 nodes 字段的两种形态：逗号分隔字符串
// （"pve1,pve2"，真实响应形态）与 JSON 字符串数组（部分场景）。
// nodes 缺失或为 null 时保持空切片；字符串形态按逗号拆分并去除空白。
func (s *PVEStorage) UnmarshalJSON(data []byte) error {
	type rawStorage struct {
		Storage string          `json:"storage"`
		Type    string          `json:"type"`
		Content string          `json:"content"`
		Shared  PveBool         `json:"shared"`
		Nodes   json.RawMessage `json:"nodes"`
	}
	var raw rawStorage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.Storage, s.Type, s.Content, s.Shared = raw.Storage, raw.Type, raw.Content, raw.Shared
	trimmed := strings.TrimSpace(string(raw.Nodes))
	switch {
	case trimmed == "" || trimmed == "null":
		// nodes 字段缺失或为 null：保持空切片。
	case strings.HasPrefix(trimmed, "["):
		if err := json.Unmarshal(raw.Nodes, &s.Nodes); err != nil {
			return err
		}
	default:
		// 逗号分隔字符串形态（真实 PVE 响应）。
		var joined string
		if err := json.Unmarshal(raw.Nodes, &joined); err != nil {
			return err
		}
		for _, n := range strings.Split(joined, ",") {
			if n = strings.TrimSpace(n); n != "" {
				s.Nodes = append(s.Nodes, n)
			}
		}
	}
	return nil
}

// ListStorage 调用集群级的 GET /storage 列出该集群的全部存储。扫描存储
// （提案 auto-scan-pve-storage）以此为数据源：一次调用即可拿到整份 cfs
// 配置（集群内各节点一致），无需逐节点聚合。content 为空串表示该存储
// 未声明内容类型，由调用方按"不支持任何内容"语义处理。
func (c *Client) ListStorage(ctx context.Context) ([]PVEStorage, error) {
	raw, err := c.doJSON(ctx, http.MethodGet, "/storage", nil, nil)
	if err != nil {
		return nil, err
	}
	storages, err := decodeData[[]PVEStorage](raw)
	if err != nil {
		return nil, fmt.Errorf("pve: list storage: %w", err)
	}
	return storages, nil
}

// ListStorageContent 调用 GET /nodes/{node}/storage/{storage}/content 列出
// 存储上指定 content 类型的条目（例如 import 目录下的云镜像文件），用于
// 扫描节点上是否已存在镜像。content 取值由调用方指定（"iso"、"import"、
// "backup"、"vztmpl" 等）；传空串时不带 content 查询参数，PVE 返回该存储
// 的全部类型。
func (c *Client) ListStorageContent(ctx context.Context, node, storage, content string) ([]StorageContent, error) {
	path := fmt.Sprintf("/nodes/%s/storage/%s/content", node, storage)
	var query neturl.Values
	if content != "" {
		query = neturl.Values{"content": {content}}
	}
	raw, err := c.doJSON(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return nil, err
	}
	items, err := decodeData[[]StorageContent](raw)
	if err != nil {
		return nil, fmt.Errorf("pve: list storage content on %s/%s: %w", node, storage, err)
	}
	// 真实 PVE 响应无 name 字段（已实测），只有 volid；对缺失 name 的条目
	// 从 volid 推导文件名，保证"扫描到的文件名 = download_url basename"
	// 的匹配语义（设计 D2）在真实环境成立。响应携带 name 时以响应值为准。
	for i := range items {
		if items[i].Name == "" {
			items[i].Name = fileNameFromVolID(items[i].VolID)
		}
	}
	return items, nil
}

// fileNameFromVolID 从卷 ID 推导文件名：取最后一个 "/" 之后的段（如
// local:import/debian-12.qcow2 → debian-12.qcow2）。volid 是 PVE 约定
// 的 {storage}:{path} 形式，纯 URL 路径语义，与文件系统无关；volid 不含
// "/" 时返回空串，由调用方按"不匹配"处理，不 panic。
func fileNameFromVolID(volID string) string {
	if i := strings.LastIndex(volID, "/"); i >= 0 && i+1 < len(volID) {
		return volID[i+1:]
	}
	return ""
}

// DownloadURL 调用 POST /nodes/{node}/storage/{storage}/download-url 让
// PVE 异步将 url 指向的文件下载到指定存储的 content 目录，并以 filename
// 命名，返回下载任务的 UPID（调用方用 WaitTask 轮询）。该端点只接受
// form-urlencoded 参数（doJSON 的 JSON body 与它不兼容），因此走 doForm
// 请求路径。PVE 官方文档中该端点的参数名即为 url，本方法的形参也因此
// 命名为 url（相应导入 net/url 时使用 neturl 别名避免遮蔽）；storage/
// content/filename 的取值（如下载目标固定为 local 存储、content=import）
// 由调用方（service 层）负责组装，本方法只透传参数。content 传空串时
// 不发送该参数（与 ListStorageContent 的空值策略一致，由 PVE 侧默认
// 处理）；url 必须非空，否则返回错误。
func (c *Client) DownloadURL(ctx context.Context, node, storage, content, filename, url string) (string, error) {
	if url == "" {
		return "", fmt.Errorf("pve: download-url: empty url")
	}
	path := fmt.Sprintf("/nodes/%s/storage/%s/download-url", node, storage)
	form := neturl.Values{}
	if content != "" {
		form.Set("content", content)
	}
	form.Set("filename", filename)
	form.Set("url", url)
	raw, err := c.doForm(ctx, http.MethodPost, path, form)
	if err != nil {
		return "", err
	}
	return decodeUPID(raw)
}

// doForm 执行一次以 application/x-www-form-urlencoded 编码请求体的 PVE
// API 调用并返回原始 "data" 负载。部分 PVE 端点（如 storage download-url）
// 只接受 form 参数，与 doJSON 的 JSON body 不兼容，因此单独实现。
//
// 复用考量：请求发送与响应解析（*UpstreamError 封装、响应大小限制、
// PVE envelope 解析）与 doJSON 完全一致；这里复制而非泛化 doJSON 的逻辑，
// 是因为 doJSON 是包内所有请求的公共路径（qemu/cluster/task），为它引入
// body 编码分派会增加每个调用点的复杂度，而 form 编码目前只有 download-url
// 一个使用者。
func (c *Client) doForm(ctx context.Context, method, path string, form neturl.Values) (json.RawMessage, error) {
	if c.initErr != nil {
		return nil, c.initErr
	}
	u := c.baseURL + path
	var bodyReader io.Reader
	if len(form) > 0 {
		bodyReader = bytes.NewBufferString(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("pve: %s %s: build request: %w", method, path, err)
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Accept", "application/json")
	if len(form) > 0 {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pve: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("pve: %s %s: read response: %w", method, path, err)
	}
	if len(raw) > maxResponseSize {
		return nil, fmt.Errorf("pve: %s %s: response exceeds %d bytes", method, path, maxResponseSize)
	}

	var env pveEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		if resp.StatusCode >= 400 {
			return nil, &UpstreamError{Method: method, Path: path, StatusCode: resp.StatusCode, Body: string(raw)}
		}
		return nil, fmt.Errorf("pve: %s %s: parse response: %w", method, path, err)
	}

	if len(env.Errors) > 0 || resp.StatusCode >= 400 {
		errors := make(map[string]string, len(env.Errors))
		for k, v := range env.Errors {
			var msg string
			if err := json.Unmarshal(v, &msg); err == nil {
				errors[k] = msg
			} else {
				errors[k] = string(v)
			}
		}
		return nil, &UpstreamError{Method: method, Path: path, StatusCode: resp.StatusCode, Body: string(raw), Errors: errors}
	}
	return env.Data, nil
}
