package pve

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultPort is the Proxmox VE management API port.
	defaultPort = 8006
	// defaultBasePath is the API2 JSON endpoint prefix.
	defaultBasePath = "api2/json"
	// defaultTimeout bounds a single HTTP request.
	defaultTimeout = 30 * time.Second
	// maxResponseSize caps how much of a PVE response body we read.
	maxResponseSize = 1 << 20 // 1 MiB

	// pveDefaultTLSInsecure documents the default TLS posture: PVE nodes
	// ship with self-signed certificates out of the box, so the client
	// skips certificate verification unless tightened via WithCAFile or
	// WithInsecure(false).
	pveDefaultTLSInsecure = true
)

// Client talks to a single Proxmox VE node over its JSON API2
// (https://{host}:8006/api2/json). It authenticates with an API token and is
// deliberately bound to one node: every higher-level operation addresses the
// node explicitly, which matches the multi-node design of the service.
type Client struct {
	baseURL    string
	authHeader string
	httpClient *http.Client
	initErr    error
}

// Option customizes a Client before it is first used. Options are applied in
// order; an Option that fails (for example a missing CA file) is recorded and
// surfaced as an error on the first request instead of being ignored.
type Option func(*Client) error

// NewClient builds a PVE API client for host (IP or hostname, no scheme) with
// the credentials stored on a pve_nodes row. A ":port" suffix on host is
// stripped so the base URL never ends up with a duplicated port
// ("https://host:8006:8006/..."); callers that need a custom port use
// WithBaseURL. IPv6 literal hosts are not supported and fail explicitly. The
// API token forms accepted for apiTokenSecret are:
//
//	"root@pam",            "spark=<secret>"         -> root@pam!spark=<secret>
//	"root@pam",            "root@pam!spark=<secret>"-> root@pam!spark=<secret>
//	"root@pam!spark",      "<secret>"               -> root@pam!spark=<secret>
//
// i.e. the token ID and secret may be split across the two arguments in any
// of the three combinations; the Authorization header is always normalized to
// the canonical form PVEAPIToken=<user>!<tokenid>=<secret>.
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
		// The error must never echo the raw credentials (apiUser or the
		// token secret): it only carries the host and a redacted user
		// prefix, so logs and client-visible errors cannot leak secrets.
		c.initErr = fmt.Errorf("pve: NewClient(host=%q, api_user=%s): cannot build API token header: need a token ID and a secret", host, redactAPIUser(apiUser))
	}
	for _, opt := range opts {
		if err := opt(c); err != nil && c.initErr == nil {
			c.initErr = err
		}
	}
	return c
}

// redactAPIUser returns a log-safe form of the API user for diagnostics: the
// identity part before any "!tokenid" suffix is kept, the token ID is masked.
// An empty input renders as "<empty>" so the message stays unambiguous.
func redactAPIUser(apiUser string) string {
	if i := strings.Index(apiUser, "!"); i >= 0 {
		apiUser = apiUser[:i]
	}
	if apiUser == "" {
		return "<empty>"
	}
	return apiUser + "!***"
}

// buildAuthHeader normalizes the credential to PVEAPIToken=<user>!<tokenid>=<secret>.
// A secret is treated as the full "user!tokenid=secret" form only when it has
// the three-segment structure (a "!" followed by a "="); a split-form secret
// that merely contains "!" (e.g. "spark=uuid!x") is not misclassified.
func buildAuthHeader(apiUser, apiTokenSecret string) string {
	user, tokenID := apiUser, ""
	if i := strings.Index(apiUser, "!"); i >= 0 {
		user, tokenID = apiUser[:i], apiUser[i+1:]
	}
	rest := apiTokenSecret
	if i := strings.Index(rest, "!"); i >= 0 {
		// Full "user!tokenid=secret" form already carries the user. The
		// "=" must come after the "!", otherwise "!" is part of a secret.
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

// stripHostPort removes a ":port" suffix from host. baseURL appends the
// default port itself, so keeping the port would produce
// "https://host:8006:8006/...". The single-colon "host:port" form is
// stripped, including an empty port ("host:"); a non-numeric suffix (such as
// a hostname in "host:name") passes through unchanged. Hosts with more than
// one colon are IPv6 literals, which are not supported (the base URL is
// always "https://{host}:8006"), so they fail with an explicit error.
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

// isValidAuthHeader checks that the header has the shape
// PVEAPIToken=<user>!<tokenid>=<secret> with a non-empty token ID and secret.
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

// WithBaseURL overrides the API base URL (testing, reverse proxies).
func WithBaseURL(baseURL string) Option {
	return func(c *Client) error {
		c.baseURL = strings.TrimSuffix(baseURL, "/")
		return nil
	}
}

// WithHTTPClient replaces the underlying HTTP client (testing).
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) error {
		if hc == nil {
			return fmt.Errorf("pve: WithHTTPClient: nil client")
		}
		c.httpClient = hc
		return nil
	}
}

// WithTimeout sets the per-request timeout. It has no effect when a custom
// client was installed via WithHTTPClient.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) error {
		c.httpClient.Timeout = d
		return nil
	}
}

// WithInsecure toggles TLS certificate verification. PVE nodes use
// self-signed certificates, so the default is insecure=true; production
// deployments can tighten this with WithCAFile.
func WithInsecure(insecure bool) Option {
	return func(c *Client) error {
		cfg := c.tlsConfig()
		cfg.InsecureSkipVerify = insecure
		return nil
	}
}

// WithCAFile verifies the node certificate against the CA bundle in path
// (PEM) and disables the default insecure skip.
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

// tlsConfig lazily creates the transport TLS config of the default client.
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

// UpstreamError carries a PVE-reported failure: the HTTP status code and the
// "errors" object from PVE's JSON envelope (param validation results,
// permission denials, ...). Network-level failures (unreachable node, TLS,
// timeout) are plain errors and can be distinguished from PVE rejections by
// asserting on *UpstreamError.
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

// pveEnvelope is the standard PVE JSON response: {"data": ..., "errors": ...}.
type pveEnvelope struct {
	Data   json.RawMessage            `json:"data"`
	Errors map[string]json.RawMessage `json:"errors"`
}

// doJSON performs a PVE API call and returns the raw "data" payload. Path is
// relative to the base URL and starts with "/" (e.g. "/nodes/pve1/qemu").
// Failures are reported as *UpstreamError; transport errors are plain errors.
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

// VersionInfo is the payload of GET /version.
type VersionInfo struct {
	Release string `json:"release"`
	RepoID  string `json:"repoid"`
	Version string `json:"version"`
}

// ProbeVersion calls GET /version and returns the node's PVE version.
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

// Ping checks node reachability and API-token validity with GET /version.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.ProbeVersion(ctx)
	return err
}

// decodeData unmarshals the raw "data" payload into a concrete type.
func decodeData[T any](raw json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("decode response data: %w", err)
	}
	return v, nil
}

// decodeUPID unmarshals the "data" payload of an endpoint that returns a task
// ID (UPID) string.
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

// isEmptyData reports whether the "data" payload is null or empty, which PVE
// uses to signal a synchronous operation that produces no task ID (e.g. PUT
// /nodes/{node}/qemu/{vmid}/config on PVE 7/8/9).
func isEmptyData(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return len(s) == 0 || s == "null" || s == `""`
}
