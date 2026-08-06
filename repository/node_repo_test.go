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
