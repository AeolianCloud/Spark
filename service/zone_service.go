package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"spark/model"
	"spark/pve"
)

// ZoneRepository is the zone data access the ZoneService depends on.
type ZoneRepository interface {
	CreateZone(ctx context.Context, name string) (*model.Zone, error)
	GetZone(ctx context.Context, id int64) (*model.Zone, error)
	ListZones(ctx context.Context) ([]model.Zone, error)
	ListZonesPage(ctx context.Context, limit, offset int) ([]model.Zone, error)
	CountZones(ctx context.Context) (int, error)
}

// NodeRepository is the node data access the ZoneService depends on.
type NodeRepository interface {
	CreateNode(ctx context.Context, node model.PVENode) (*model.PVENode, error)
	GetNode(ctx context.Context, id int64) (*model.PVENode, error)
	ListNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error)
	ListEnabledNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error)
	UpdateNode(ctx context.Context, node model.PVENode) (*model.PVENode, error)
}

// ZoneService implements the business rules for zones and their PVE nodes.
type ZoneService struct {
	zoneRepo ZoneRepository
	nodeRepo NodeRepository
}

// NewZoneService creates a ZoneService backed by the given repositories.
func NewZoneService(zoneRepo ZoneRepository, nodeRepo NodeRepository) *ZoneService {
	return &ZoneService{zoneRepo: zoneRepo, nodeRepo: nodeRepo}
}

// KindNodeUnavailable marks "no candidate node is reachable". The value sits
// outside the iota range of the shared kinds in errors.go (owned by other
// batches) to avoid coupling this file to their edits.
const KindNodeUnavailable ErrorKind = 100

// nodeUnavailablef builds a KindNodeUnavailable service error.
func nodeUnavailablef(format string, args ...any) *Error {
	return &Error{Kind: KindNodeUnavailable, Message: fmt.Sprintf(format, args...)}
}

// nodeProbeTimeout bounds a single reachability probe against a candidate
// node.
const nodeProbeTimeout = 5 * time.Second

// nodeProbeBudget bounds the whole reachability sweep across all candidate
// nodes, so N probes never accumulate into N x nodeProbeTimeout.
const nodeProbeBudget = 15 * time.Second

// CreateZone creates a zone. The name is required and must be unique; the
// check is a best-effort scan (the zones table has no unique constraint) and
// two racing creates could still slip through, which is accepted for v1.
func (s *ZoneService) CreateZone(ctx context.Context, name string) (*model.Zone, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, badRequestf("zone name is required")
	}
	zones, err := s.zoneRepo.ListZones(ctx)
	if err != nil {
		return nil, fmt.Errorf("create zone: list zones: %w", err)
	}
	for _, z := range zones {
		if z.Name == name {
			return nil, conflictf("zone %q already exists", name)
		}
	}
	zone, err := s.zoneRepo.CreateZone(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("create zone: %w", err)
	}
	return zone, nil
}

// ZoneWithNodes pairs a zone with its nodes for list responses.
type ZoneWithNodes struct {
	Zone  model.Zone
	Nodes []model.PVENode
}

// ListZones returns one page of zones together with their nodes, satisfying
// the zones spec's "return all zones and their node information". The
// pagination unit is the zone: the page holds limit zones (offset skips the
// first offset zones in id order), each with its full, unpaginated node
// list — a zone embeds few nodes, so only the zone rows are paged. total is
// the total number of zones, independent of the page.
func (s *ZoneService) ListZones(ctx context.Context, limit, offset int) ([]ZoneWithNodes, int, error) {
	zones, err := s.zoneRepo.ListZonesPage(ctx, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list zones: %w", err)
	}
	out := make([]ZoneWithNodes, 0, len(zones))
	for _, z := range zones {
		nodes, err := s.nodeRepo.ListNodesByZone(ctx, z.ID)
		if err != nil {
			return nil, 0, fmt.Errorf("list zones: nodes of zone %d: %w", z.ID, err)
		}
		out = append(out, ZoneWithNodes{Zone: z, Nodes: nodes})
	}
	total, err := s.zoneRepo.CountZones(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list zones: count: %w", err)
	}
	return out, total, nil
}

// CreateNode registers a node in a zone. The zone must exist, the name must
// be unique within the zone, and host/api_user/api_token are required. The
// API token is stored in plain text by design.
func (s *ZoneService) CreateNode(ctx context.Context, zoneID int64, name, host, apiUser, apiToken string, enabled *bool) (*model.PVENode, error) {
	name = strings.TrimSpace(name)
	host = strings.TrimSpace(host)
	apiUser = strings.TrimSpace(apiUser)
	apiToken = strings.TrimSpace(apiToken)
	switch {
	case name == "":
		return nil, badRequestf("node name is required")
	case host == "":
		return nil, badRequestf("node host is required")
	case apiUser == "":
		return nil, badRequestf("node api_user is required")
	case apiToken == "":
		return nil, badRequestf("node api_token is required")
	}

	if _, err := s.zoneRepo.GetZone(ctx, zoneID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("zone %d not found", zoneID)
		}
		return nil, fmt.Errorf("create node: check zone: %w", err)
	}
	nodes, err := s.nodeRepo.ListNodesByZone(ctx, zoneID)
	if err != nil {
		return nil, fmt.Errorf("create node: list nodes: %w", err)
	}
	for _, n := range nodes {
		if n.Name == name {
			return nil, conflictf("node %q already exists in zone %d", name, zoneID)
		}
	}

	node := model.PVENode{
		ZoneID: zoneID, Name: name, Host: host, APIUser: apiUser,
		APITokenSecret: apiToken, Enabled: true,
	}
	if enabled != nil {
		node.Enabled = *enabled
	}
	created, err := s.nodeRepo.CreateNode(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("create node: %w", err)
	}
	return created, nil
}

// UpdateNode replaces the editable fields of the node (name/host/api_user/
// enabled). api_token is optional: an empty value keeps the stored secret.
// The second return value reports whether the token was replaced, so handlers
// can answer api_token_set without ever echoing the secret.
func (s *ZoneService) UpdateNode(ctx context.Context, id int64, name, host, apiUser, apiToken string, enabled *bool) (*model.PVENode, bool, error) {
	name = strings.TrimSpace(name)
	host = strings.TrimSpace(host)
	apiUser = strings.TrimSpace(apiUser)
	apiToken = strings.TrimSpace(apiToken)
	switch {
	case name == "":
		return nil, false, badRequestf("node name is required")
	case host == "":
		return nil, false, badRequestf("node host is required")
	case apiUser == "":
		return nil, false, badRequestf("node api_user is required")
	}

	existing, err := s.nodeRepo.GetNode(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, notFoundf("node %d not found", id)
		}
		return nil, false, fmt.Errorf("update node: get: %w", err)
	}
	if name != existing.Name {
		nodes, err := s.nodeRepo.ListNodesByZone(ctx, existing.ZoneID)
		if err != nil {
			return nil, false, fmt.Errorf("update node: list nodes: %w", err)
		}
		for _, n := range nodes {
			if n.Name == name && n.ID != id {
				return nil, false, conflictf("node %q already exists in zone %d", name, existing.ZoneID)
			}
		}
	}

	node := *existing
	node.Name, node.Host, node.APIUser = name, host, apiUser
	tokenChanged := apiToken != ""
	if tokenChanged {
		node.APITokenSecret = apiToken
	}
	if enabled != nil {
		node.Enabled = *enabled
	}
	updated, err := s.nodeRepo.UpdateNode(ctx, node)
	if err != nil {
		return nil, false, fmt.Errorf("update node: %w", err)
	}
	return updated, tokenChanged, nil
}

// ListNodesByZone returns the nodes of a zone; the zone must exist.
func (s *ZoneService) ListNodesByZone(ctx context.Context, zoneID int64) ([]model.PVENode, error) {
	if _, err := s.zoneRepo.GetZone(ctx, zoneID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFoundf("zone %d not found", zoneID)
		}
		return nil, fmt.Errorf("list nodes: check zone: %w", err)
	}
	nodes, err := s.nodeRepo.ListNodesByZone(ctx, zoneID)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	return nodes, nil
}

// SelectReachableNode probes the candidate nodes in order and returns the
// first one whose PVE API answers (each probe is bounded by nodeProbeTimeout;
// the whole sweep is bounded by nodeProbeBudget so many dead nodes cannot
// accumulate into a long stall). A node that fails (unreachable, TLS,
// timeout or invalid token) is skipped and the failure is logged at debug
// level with the error as reported by the PVE client, which never embeds
// credentials (see pve.NewClient redaction). When none is reachable a
// KindNodeUnavailable error is returned. VM creation (task 7.1) uses this to
// pick the deployment node.
func SelectReachableNode(ctx context.Context, nodes []model.PVENode) (model.PVENode, error) {
	return selectReachableNode(ctx, nodes, func(host, apiUser, apiTokenSecret string) *pve.Client {
		return pve.NewClient(host, apiUser, apiTokenSecret, pve.WithTimeout(nodeProbeTimeout))
	})
}

// selectReachableNode is SelectReachableNode with the client factory injected
// so tests can point the probes at fake servers.
func selectReachableNode(ctx context.Context, nodes []model.PVENode, newClient func(host, apiUser, apiTokenSecret string) *pve.Client) (model.PVENode, error) {
	probeCtx, cancel := context.WithTimeout(ctx, nodeProbeBudget)
	defer cancel()

	for _, n := range nodes {
		client := newClient(n.Host, n.APIUser, n.APITokenSecret)
		if err := client.Ping(probeCtx); err != nil {
			slog.Debug("node unreachable, skipping candidate",
				"node", n.Name,
				"host", n.Host,
				"reason", err,
			)
			continue
		}
		return n, nil
	}
	return model.PVENode{}, nodeUnavailablef("no reachable node among %d candidate(s)", len(nodes))
}
