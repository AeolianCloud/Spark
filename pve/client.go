package pve

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultPort 是 Proxmox VE 管理 API 端口。
	defaultPort = 8006
	// defaultBasePath 是 API2 JSON 端点前缀。
	defaultBasePath = "api2/json"
	// defaultTimeout 限制单个 HTTP 请求的时长。
	defaultTimeout = 30 * time.Second
	// maxResponseSize 限制我们读取的 PVE 响应体的最大字节数。
	maxResponseSize = 1 << 20 // 1 MiB

	// pveDefaultTLSInsecure 说明默认的 TLS 姿态：PVE 节点出厂自带自签名
	// 证书，因此客户端默认跳过证书校验，除非通过 WithCAFile 或
	// WithInsecure(false) 收紧。
	pveDefaultTLSInsecure = true
)

// Client 通过 JSON API2 (https://{host}:8006/api2/json) 与单个 Proxmox VE
// 节点通信。它使用 API 令牌认证，并有意地绑定到单个节点：所有上层操作都
// 显式指定节点，这与服务的多节点设计相匹配。
type Client struct {
	baseURL    string
	authHeader string
	httpClient *http.Client
	initErr    error
}

// Option 在 Client 首次使用前对其进行定制。Options 按顺序应用；失败的
// Option（例如 CA 文件缺失）会被记录下来，并在首次请求时以错误形式呈现，
// 而不是被忽略。
type Option func(*Client) error

// NewClient 为 host（IP 或主机名，不带 scheme）构建 PVE API 客户端，凭证
// 取自 pve_nodes 表的一行记录。host 上的 ":port" 后缀会被剥离，确保 base
// URL 不会出现重复端口（"https://host:8006:8006/..."）；需要自定义端口的
// 调用方请使用 WithPort。不支持 IPv6 字面量主机，会显式报错。apiTokenSecret
// 可接受的 API 令牌形式有：
//
//	"root@pam",            "spark=<secret>"         -> root@pam!spark=<secret>
//	"root@pam",            "root@pam!spark=<secret>"-> root@pam!spark=<secret>
//	"root@pam!spark",      "<secret>"               -> root@pam!spark=<secret>
//
// 即令牌 ID 与密钥可以三种组合方式拆分到两个参数中；Authorization 头始终
// 规范化为 PVEAPIToken=<user>!<tokenid>=<secret> 形式。
func NewClient(host, apiUser, apiTokenSecret string, opts ...Option) *Client {
	host = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(host), "https://"), "http://"), "/")
	host, hostErr := stripHostPort(host)
	c := &Client{
		baseURL:    fmt.Sprintf("https://%s:%d/%s", host, defaultPort, defaultBasePath),
		authHeader: buildAuthHeader(apiUser, apiTokenSecret),
		httpClient: &http.Client{
			Timeout: defaultTimeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: pveDefaultTLSInsecure},
			},
		},
	}
	if hostErr != nil {
		c.initErr = hostErr
	} else if host == "" {
		c.initErr = fmt.Errorf("pve: NewClient: empty host")
	}
	if !isValidAuthHeader(c.authHeader) {
		// 该错误绝不能回显原始凭证（apiUser 或令牌密钥）：它只携带
		// 主机名和一个脱敏后的用户前缀，这样日志和客户端可见的错误
		// 就不会泄露机密。
		c.initErr = fmt.Errorf("pve: NewClient(host=%q, api_user=%s): cannot build API token header: need a token ID and a secret", host, redactAPIUser(apiUser))
	}
	for _, opt := range opts {
		if err := opt(c); err != nil && c.initErr == nil {
			c.initErr = err
		}
	}
	return c
}

// redactAPIUser 返回用于诊断的 API 用户日志安全形式：保留任何
// "!tokenid" 后缀之前的主体部分，令牌 ID 被掩码处理。空输入渲染为
// "<empty>"，以保证消息无歧义。
func redactAPIUser(apiUser string) string {
	if i := strings.Index(apiUser, "!"); i >= 0 {
		apiUser = apiUser[:i]
	}
	if apiUser == "" {
		return "<empty>"
	}
	return apiUser + "!***"
}

// buildAuthHeader 将凭证规范化为 PVEAPIToken=<user>!<tokenid>=<secret>。
// 只有 secret 具有三段式结构（"!" 后跟 "="）时才被视为完整的
// "user!tokenid=secret" 形式；仅包含 "!" 的拆分形式 secret
// （例如 "spark=uuid!x"）不会被误判。
func buildAuthHeader(apiUser, apiTokenSecret string) string {
	user, tokenID := apiUser, ""
	if i := strings.Index(apiUser, "!"); i >= 0 {
		user, tokenID = apiUser[:i], apiUser[i+1:]
	}
	rest := apiTokenSecret
	if i := strings.Index(rest, "!"); i >= 0 {
		// 完整的 "user!tokenid=secret" 形式已携带用户。"=" 必须位于 "!"
		// 之后，否则 "!" 只是密钥的一部分。
		if j := strings.Index(rest, "="); j > i {
			return "PVEAPIToken=" + rest
		}
	}
	if i := strings.Index(rest, "="); i >= 0 {
		tokenID = rest[:i]
		rest = rest[i+1:]
	}
	return "PVEAPIToken=" + user + "!" + tokenID + "=" + rest
}

// stripHostPort 从 host 中移除 ":port" 后缀。baseURL 自身会拼接默认端口，
// 因此保留端口会产生 "https://host:8006:8006/..."。单冒号的 "host:port"
// 形式会被剥离，包括空端口（"host:"）；非数字后缀（例如 "host:name" 中的
// 主机名）会原样保留。含多个冒号的主机是 IPv6 字面量，不受支持（base URL
// 始终是 "https://{host}:8006"），因此会返回显式错误。
func stripHostPort(host string) (string, error) {
	if strings.Count(host, ":") > 1 {
		return "", fmt.Errorf("pve: NewClient: IPv6 host %q is not supported", host)
	}
	i := strings.LastIndex(host, ":")
	if i < 0 {
		return host, nil
	}
	port := host[i+1:]
	if port != "" {
		if _, err := strconv.Atoi(port); err != nil {
			return host, nil
		}
	}
	return host[:i], nil
}

// isValidAuthHeader 检查头部是否符合 PVEAPIToken=<user>!<tokenid>=<secret>
// 的形式，且令牌 ID 与密钥均非空。
func isValidAuthHeader(header string) bool {
	rest, ok := strings.CutPrefix(header, "PVEAPIToken=")
	if !ok {
		return false
	}
	i := strings.Index(rest, "!")
	if i <= 0 {
		return false
	}
	j := strings.Index(rest, "=")
	return j > i+1 && j < len(rest)-1
}

// WithBaseURL 覆盖 API base URL（用于测试、反向代理场景）。
func WithBaseURL(baseURL string) Option {
	return func(c *Client) error {
		c.baseURL = strings.TrimSuffix(baseURL, "/")
		return nil
	}
}

// WithPort 覆盖 base URL 中的 API 端口（默认 8006）。port 为 0 时
// 忽略，保持默认端口；超出合法范围（1-65535）的取值会显式报错，
// 而不是被静默忽略。它通过 url.Parse 直接改写 c.baseURL 的端口部分，
// 因此与 WithBaseURL 同时使用时，后应用的 Option 生效（Options 按
// 顺序应用），任意顺序都不会产生 "https://host:8006:8007/..." 这种
// 双端口。
func WithPort(port int) Option {
	return func(c *Client) error {
		if port == 0 {
			return nil
		}
		if port < 0 || port > 65535 {
			return fmt.Errorf("pve: WithPort: invalid port %d (must be 0 or 1-65535)", port)
		}
		u, err := url.Parse(c.baseURL)
		if err != nil {
			return fmt.Errorf("pve: WithPort: parse base URL: %w", err)
		}
		u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(port))
		c.baseURL = u.String()
		return nil
	}
}

// WithHTTPClient 替换底层 HTTP 客户端（用于测试）。
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) error {
		if hc == nil {
			return fmt.Errorf("pve: WithHTTPClient: nil client")
		}
		c.httpClient = hc
		return nil
	}
}

// WithTimeout 设置每个请求的超时时长。当已通过 WithHTTPClient 安装了
// 自定义客户端时，此选项不生效。
func WithTimeout(d time.Duration) Option {
	return func(c *Client) error {
		c.httpClient.Timeout = d
		return nil
	}
}

// WithInsecure 切换 TLS 证书校验。PVE 节点使用自签名证书，因此默认是
// insecure=true；生产部署可通过 WithCAFile 收紧。
func WithInsecure(insecure bool) Option {
	return func(c *Client) error {
		cfg := c.tlsConfig()
		cfg.InsecureSkipVerify = insecure
		return nil
	}
}

// WithCAFile 使用 path（PEM 格式）中的 CA 证书包校验节点证书，并关闭
// 默认的不安全跳过行为。
func WithCAFile(path string) Option {
	return func(c *Client) error {
		pem, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("pve: WithCAFile: read %s: %w", path, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return fmt.Errorf("pve: WithCAFile: %s: no PEM certificates found", path)
		}
		cfg := c.tlsConfig()
		cfg.RootCAs = pool
		cfg.InsecureSkipVerify = false
		return nil
	}
}

// tlsConfig 惰性创建默认客户端的传输层 TLS 配置。
func (c *Client) tlsConfig() *tls.Config {
	transport, ok := c.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		transport = &http.Transport{}
		c.httpClient.Transport = transport
	}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	return transport.TLSClientConfig
}

// UpstreamError 承载 PVE 报告的错误：HTTP 状态码以及 PVE JSON 封装中的
// "errors" 对象（参数校验结果、权限拒绝等）。网络层故障（节点不可达、
// TLS、超时）是普通 error，可以通过断言 *UpstreamError 与 PVE 拒绝
// 区分开。
type UpstreamError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
	Errors     map[string]string
}

func (e *UpstreamError) Error() string {
	msg := strings.TrimSpace(e.Body)
	if len(e.Errors) > 0 {
		keys := make([]string, 0, len(e.Errors))
		for k := range e.Errors {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s: %s", k, e.Errors[k]))
		}
		msg = "errors: " + strings.Join(parts, ", ")
	}
	if msg == "" {
		msg = "empty response"
	}
	return fmt.Sprintf("pve: %s %s: status %d: %s", e.Method, e.Path, e.StatusCode, msg)
}

// pveEnvelope 是标准的 PVE JSON 响应：{"data": ..., "errors": ...}。
type pveEnvelope struct {
	Data   json.RawMessage            `json:"data"`
	Errors map[string]json.RawMessage `json:"errors"`
}

// doJSON 执行一次 PVE API 调用并返回原始 "data" 负载。Path 相对于 base
// URL，以 "/" 开头（例如 "/nodes/pve1/qemu"）。失败以 *UpstreamError 报告；
// 传输层错误是普通 error。
func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body any) (json.RawMessage, error) {
	if c.initErr != nil {
		return nil, c.initErr
	}
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("pve: %s %s: encode body: %w", method, path, err)
		}
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("pve: %s %s: build request: %w", method, path, err)
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
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

// VersionInfo 是 GET /version 的响应负载。
type VersionInfo struct {
	Release string `json:"release"`
	RepoID  string `json:"repoid"`
	Version string `json:"version"`
}

// ProbeVersion 调用 GET /version 并返回节点的 PVE 版本。
func (c *Client) ProbeVersion(ctx context.Context) (*VersionInfo, error) {
	raw, err := c.doJSON(ctx, http.MethodGet, "/version", nil, nil)
	if err != nil {
		return nil, err
	}
	v, err := decodeData[VersionInfo](raw)
	if err != nil {
		return nil, fmt.Errorf("pve: probe version: %w", err)
	}
	return &v, nil
}

// Ping 通过 GET /version 检查节点可达性与 API 令牌有效性。
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.ProbeVersion(ctx)
	return err
}

// NodeInfo 是 GET /nodes 响应中单个集群节点的信息。
type NodeInfo struct {
	// Node 是 PVE 集群节点名，登记节点时用它探测集群真实节点名列表。
	Node string `json:"node"`
	// Status 是节点的在线状态（例如 "online"），部分场景可能缺失，
	// 因此是可选的。
	Status string `json:"status"`
}

// ListNodes 调用 GET /nodes 探测 PVE 集群节点名列表，登记节点时用来
// 校验业务名与集群真实节点名是否一致，避免不一致导致 595 错误。
// 请求需要 Authorization 头（doJSON 已处理）；无有效 token 时 PVE
// 返回 401，以 *UpstreamError 形式呈现。
func (c *Client) ListNodes(ctx context.Context) ([]NodeInfo, error) {
	raw, err := c.doJSON(ctx, http.MethodGet, "/nodes", nil, nil)
	if err != nil {
		return nil, err
	}
	nodes, err := decodeData[[]NodeInfo](raw)
	if err != nil {
		return nil, fmt.Errorf("pve: list nodes: %w", err)
	}
	return nodes, nil
}

// decodeData 将原始 "data" 负载反序列化为具体类型。
func decodeData[T any](raw json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("decode response data: %w", err)
	}
	return v, nil
}

// decodeUPID 反序列化返回任务 ID（UPID）字符串的端点的 "data" 负载。
func decodeUPID(raw json.RawMessage) (string, error) {
	upid, err := decodeData[string](raw)
	if err != nil {
		return "", err
	}
	if upid == "" {
		return "", fmt.Errorf("empty task ID in response")
	}
	return upid, nil
}

// isEmptyData 报告 "data" 负载是否为 null 或空；PVE 用它来表示不产生
// 任务 ID 的同步操作（例如 PVE 7/8/9 上的 PUT
// /nodes/{node}/qemu/{vmid}/config）。
func isEmptyData(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return len(s) == 0 || s == "null" || s == `""`
}
