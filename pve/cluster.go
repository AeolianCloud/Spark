package pve

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// NextVMID 向集群请求下一个空闲 VMID（GET /cluster/nextid）。PVE 以
// JSON 字符串形式返回候选值（例如 "100"），因此在转换为 int 前负载被
// 解析为字符串。VM 创建链路（服务批次 7）在部署时用它分配 VMID，而不是
// 让客户端自行选择。
func (c *Client) NextVMID(ctx context.Context) (int, error) {
	raw, err := c.doJSON(ctx, http.MethodGet, "/cluster/nextid", nil, nil)
	if err != nil {
		return 0, err
	}
	s, err := decodeData[string](raw)
	if err != nil {
		return 0, fmt.Errorf("pve: next vmid: %w", err)
	}
	vmid, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("pve: next vmid: %q is not an integer", s)
	}
	return vmid, nil
}
