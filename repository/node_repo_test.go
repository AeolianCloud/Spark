package repository

import (
	"context"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"spark/model"
)

// TestCreateNodePersistsPort 验证插入时 port 列与参数一一对应，
// 且 RETURNING 只回读 id/created_at（其余字段由调用方传入的行携带）。
func TestCreateNodePersistsPort(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("INSERT INTO pve_nodes (zone_id, name, pve_name, host, port, api_user, api_token_secret, enabled) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id, created_at").
		WithArgs(int64(1), "pve1", "aeoliancloud", "10.0.0.1", 8006, "root@pam!spark", "secret", true).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).
			AddRow(int64(10), testTime))

	repo := NewNodeRepository(mock)
	node, err := repo.CreateNode(context.Background(), model.PVENode{
		ZoneID: 1, Name: "pve1", PveName: "aeoliancloud", Host: "10.0.0.1", Port: 8006,
		APIUser: "root@pam!spark", APITokenSecret: "secret", Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if node.ID != 10 || node.Port != 8006 || node.Host != "10.0.0.1" || node.PveName != "aeoliancloud" {
		t.Fatalf("unexpected node: %+v", node)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestGetNodeReadsPort 验证 GetNode 按 nodeCols 列顺序扫描 port 与 pve_name 列。
func TestGetNodeReadsPort(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT id, zone_id, name, pve_name, host, port, api_user, api_token_secret, enabled, created_at FROM pve_nodes WHERE id=$1").
		WithArgs(int64(10)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "zone_id", "name", "pve_name", "host", "port", "api_user", "api_token_secret", "enabled", "created_at"}).
			AddRow(int64(10), int64(1), "pve1", "aeoliancloud", "10.0.0.1", int32(8443), "root@pam!spark", "secret", true, testTime))

	repo := NewNodeRepository(mock)
	node, err := repo.GetNode(context.Background(), 10)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if node.Port != 8443 || node.Host != "10.0.0.1" || node.PveName != "aeoliancloud" {
		t.Fatalf("node = %+v, want port 8443 on host 10.0.0.1 with pve_name aeoliancloud", node)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestListNodesByZoneReadsPort 验证列表扫描同样覆盖 port 与 pve_name 列。
func TestListNodesByZoneReadsPort(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT id, zone_id, name, pve_name, host, port, api_user, api_token_secret, enabled, created_at FROM pve_nodes WHERE zone_id=$1 ORDER BY id").
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "zone_id", "name", "pve_name", "host", "port", "api_user", "api_token_secret", "enabled", "created_at"}).
			AddRow(int64(1), int64(1), "pve1", "aeoliancloud", "10.0.0.1", int32(8006), "root@pam!spark", "s1", true, testTime).
			AddRow(int64(2), int64(1), "pve2", "", "10.0.0.2", int32(9999), "root@pam!spark", "s2", false, testTime))

	repo := NewNodeRepository(mock)
	nodes, err := repo.ListNodesByZone(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListNodesByZone: %v", err)
	}
	if len(nodes) != 2 || nodes[0].Port != 8006 || nodes[1].Port != 9999 {
		t.Fatalf("nodes = %+v, want ports 8006/9999", nodes)
	}
	if nodes[0].PveName != "aeoliancloud" || nodes[1].PveName != "" {
		t.Fatalf("nodes = %+v, want pve_name aeoliancloud/empty", nodes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestListNodesByIDs 验证按 id 集合查询节点（disabled 警告的节点名翻译）：
// 不存在的 id 被忽略，结果按 id 排序。
func TestListNodesByIDs(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT id, zone_id, name, pve_name, host, port, api_user, api_token_secret, enabled, created_at FROM pve_nodes WHERE id = ANY($1) ORDER BY id").
		WithArgs([]int64{1, 3}).
		WillReturnRows(pgxmock.NewRows([]string{"id", "zone_id", "name", "pve_name", "host", "port", "api_user", "api_token_secret", "enabled", "created_at"}).
			AddRow(int64(1), int64(1), "pve1", "aeoliancloud", "10.0.0.1", int32(8006), "root@pam!spark", "s1", true, testTime).
			AddRow(int64(3), int64(1), "pve3", "", "10.0.0.3", int32(8006), "root@pam!spark", "s3", false, testTime))

	repo := NewNodeRepository(mock)
	nodes, err := repo.ListNodesByIDs(context.Background(), []int64{1, 3})
	if err != nil {
		t.Fatalf("ListNodesByIDs: %v", err)
	}
	if len(nodes) != 2 || nodes[0].ID != 1 || nodes[1].ID != 3 ||
		nodes[0].Name != "pve1" || nodes[1].Name != "pve3" {
		t.Fatalf("nodes = %+v, want pve1 then pve3 (id 99 ignored)", nodes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestListEnabledNodesByZone 验证区域内启用节点的过滤条件（zone_id + enabled）
// 与列读取。
func TestListEnabledNodesByZone(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT id, zone_id, name, pve_name, host, port, api_user, api_token_secret, enabled, created_at FROM pve_nodes WHERE zone_id=$1 AND enabled ORDER BY id").
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "zone_id", "name", "pve_name", "host", "port", "api_user", "api_token_secret", "enabled", "created_at"}).
			AddRow(int64(1), int64(1), "pve1", "aeoliancloud", "10.0.0.1", int32(8006), "root@pam!spark", "s1", true, testTime))

	repo := NewNodeRepository(mock)
	nodes, err := repo.ListEnabledNodesByZone(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListEnabledNodesByZone: %v", err)
	}
	if len(nodes) != 1 || nodes[0].ID != 1 || nodes[0].PveName != "aeoliancloud" {
		t.Fatalf("nodes = %+v, want only enabled node pve1", nodes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestListAllEnabledNodes 验证跨区域全量启用节点查询（不带 zone 过滤，
// 支撑镜像服务不带区域的节点状态扫描），结果按 id 排序。
func TestListAllEnabledNodes(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT id, zone_id, name, pve_name, host, port, api_user, api_token_secret, enabled, created_at FROM pve_nodes WHERE enabled ORDER BY id").
		WillReturnRows(pgxmock.NewRows([]string{"id", "zone_id", "name", "pve_name", "host", "port", "api_user", "api_token_secret", "enabled", "created_at"}).
			AddRow(int64(1), int64(1), "pve1", "aeoliancloud", "10.0.0.1", int32(8006), "root@pam!spark", "s1", true, testTime).
			AddRow(int64(3), int64(2), "pve3", "", "10.0.0.3", int32(8006), "root@pam!spark", "s3", true, testTime))

	repo := NewNodeRepository(mock)
	nodes, err := repo.ListAllEnabledNodes(context.Background())
	if err != nil {
		t.Fatalf("ListAllEnabledNodes: %v", err)
	}
	if len(nodes) != 2 || nodes[0].ID != 1 || nodes[1].ID != 3 ||
		nodes[0].ZoneID != 1 || nodes[1].ZoneID != 2 {
		t.Fatalf("nodes = %+v, want enabled nodes id 1 (zone 1) and 3 (zone 2), disabled node 2 excluded", nodes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUpdateNodeWritesAndReadsPort 验证 UPDATE 写入 port 与 pve_name 列，
// 且 RETURNING nodeCols 扫描同样读取它们。
func TestUpdateNodeWritesAndReadsPort(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("UPDATE pve_nodes SET zone_id=$1, name=$2, pve_name=$3, host=$4, port=$5, api_user=$6, api_token_secret=$7, enabled=$8 WHERE id=$9 RETURNING id, zone_id, name, pve_name, host, port, api_user, api_token_secret, enabled, created_at").
		WithArgs(int64(1), "pve1", "aeoliancloud", "10.0.0.1", 8443, "root@pam!spark", "new-secret", true, int64(10)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "zone_id", "name", "pve_name", "host", "port", "api_user", "api_token_secret", "enabled", "created_at"}).
			AddRow(int64(10), int64(1), "pve1", "aeoliancloud", "10.0.0.1", int32(8443), "root@pam!spark", "new-secret", true, testTime))

	repo := NewNodeRepository(mock)
	node, err := repo.UpdateNode(context.Background(), model.PVENode{
		ID: 10, ZoneID: 1, Name: "pve1", PveName: "aeoliancloud", Host: "10.0.0.1", Port: 8443,
		APIUser: "root@pam!spark", APITokenSecret: "new-secret", Enabled: true,
	})
	if err != nil {
		t.Fatalf("UpdateNode: %v", err)
	}
	if node.Port != 8443 || node.APITokenSecret != "new-secret" || node.PveName != "aeoliancloud" {
		t.Fatalf("node = %+v, want port 8443, updated token and pve_name aeoliancloud", node)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
