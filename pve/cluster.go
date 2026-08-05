package pve

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// NextVMID asks the cluster for the next free VMID (GET /cluster/nextid).
// PVE returns the candidate as a JSON string (e.g. "100"), so the payload is
// parsed as a string before converting to int. The VM create chain (service
// batch 7) uses this to assign the VMID at provisioning time instead of
// asking the client to pick one.
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
