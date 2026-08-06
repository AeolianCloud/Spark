package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"spark/model"
)

var testTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// newMockPool 返回一个 SQL 期望必须与实际语句完全一致的 pgxmock 池
// （不允许空白/正则松弛），因此这些测试同时也钉死了并发关键
// 分配 SQL 的形态。
func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	return mock
}

func TestAllocateFreeIPClaimsAtomically(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectBegin()
	mock.ExpectQuery(selectFreeIPSQL).WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "pool_id", "ip", "status", "vm_id", "updated_at"}).
			AddRow(int64(10), int64(1), "10.0.0.5", "free", nil, testTime))
	// 领取必须重新检查 status='free'（由 claimFreeIPSQL 本身断言，
	// 并被 mock 精确匹配）——正是这个 WHERE 守卫保证了并发分配的安全。
	mock.ExpectExec(claimFreeIPSQL).WithArgs(int64(10), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	repo := NewIPPoolRepository(mock)
	ip, err := repo.AllocateFreeIP(context.Background(), 1, nil)
	if err != nil {
		t.Fatalf("AllocateFreeIP: %v", err)
	}
	if ip.ID != 10 || ip.IP != "10.0.0.5" {
		t.Fatalf("unexpected ip: %+v", ip)
	}
	if ip.Status != model.IPStatusUsed {
		t.Fatalf("status = %q, want %q", ip.Status, model.IPStatusUsed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestAllocateFreeIPLinksVM(t *testing.T) {
	mock := newMockPool(t)
	vmID := int64(42)
	mock.ExpectBegin()
	mock.ExpectQuery(selectFreeIPSQL).WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "pool_id", "ip", "status", "vm_id", "updated_at"}).
			AddRow(int64(10), int64(1), "10.0.0.5", "free", nil, testTime))
	mock.ExpectExec(claimFreeIPSQL).WithArgs(int64(10), &vmID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	repo := NewIPPoolRepository(mock)
	ip, err := repo.AllocateFreeIP(context.Background(), 1, &vmID)
	if err != nil {
		t.Fatalf("AllocateFreeIP: %v", err)
	}
	if ip.VMID == nil || *ip.VMID != vmID {
		t.Fatalf("vm_id = %v, want %d", ip.VMID, vmID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestAllocateFreeIPNoFreeAddress(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectBegin()
	mock.ExpectQuery(selectFreeIPSQL).WithArgs(int64(1)).WillReturnError(pgx.ErrNoRows)
	mock.ExpectRollback()

	repo := NewIPPoolRepository(mock)
	_, err := repo.AllocateFreeIP(context.Background(), 1, nil)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("err = %v, want pgx.ErrNoRows", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestAllocateFreeIPLostRaceReturnsRetry(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectBegin()
	mock.ExpectQuery(selectFreeIPSQL).WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "pool_id", "ip", "status", "vm_id", "updated_at"}).
			AddRow(int64(10), int64(1), "10.0.0.5", "free", nil, testTime))
	// 0 行受影响：并发事务抢先领取了该地址。
	mock.ExpectExec(claimFreeIPSQL).WithArgs(int64(10), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectRollback()

	repo := NewIPPoolRepository(mock)
	_, err := repo.AllocateFreeIP(context.Background(), 1, nil)
	if !errors.Is(err, ErrAllocationRetry) {
		t.Fatalf("err = %v, want ErrAllocationRetry", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestCreatePoolWithIPs(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO ip_pools (zone_id, name, network_cidr, gateway, dns) VALUES ($1, $2, $3, $4, $5) RETURNING id, created_at").
		WithArgs(int64(1), "default", "10.0.0.0/30", "10.0.0.1", "1.1.1.1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).AddRow(int64(7), testTime))
	mock.ExpectExec("INSERT INTO ips (pool_id, ip) VALUES ($1, $2), ($1, $3)").
		WithArgs(int64(7), "10.0.0.2", "10.0.0.3").
		WillReturnResult(pgxmock.NewResult("INSERT", 2))
	mock.ExpectCommit()

	repo := NewIPPoolRepository(mock)
	pool, err := repo.CreatePoolWithIPs(context.Background(),
		model.IPPool{ZoneID: 1, Name: "default", NetworkCIDR: "10.0.0.0/30", Gateway: "10.0.0.1", DNS: "1.1.1.1"},
		[]model.IP{{IP: "10.0.0.2"}, {IP: "10.0.0.3"}})
	if err != nil {
		t.Fatalf("CreatePoolWithIPs: %v", err)
	}
	if pool.ID != 7 || pool.ZoneID != 1 || pool.Name != "default" {
		t.Fatalf("unexpected pool: %+v", pool)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestCreatePoolWithIPsOverlappingCIDRConflict(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO ip_pools (zone_id, name, network_cidr, gateway, dns) VALUES ($1, $2, $3, $4, $5) RETURNING id, created_at").
		WithArgs(int64(1), "default", "10.0.0.0/30", "10.0.0.1", "").
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).AddRow(int64(7), testTime))
	// 一个全局唯一的地址已被其他池占用。
	mock.ExpectExec("INSERT INTO ips (pool_id, ip) VALUES ($1, $2)").
		WithArgs(int64(7), "10.0.0.2").
		WillReturnError(&pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"})
	mock.ExpectRollback()

	repo := NewIPPoolRepository(mock)
	_, err := repo.CreatePoolWithIPs(context.Background(),
		model.IPPool{ZoneID: 1, Name: "default", NetworkCIDR: "10.0.0.0/30", Gateway: "10.0.0.1", DNS: ""},
		[]model.IP{{IP: "10.0.0.2"}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestSetPoolNodesReplacesWhitelist(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM ip_pool_nodes WHERE ip_pool_id=$1").WithArgs(int64(3)).
		WillReturnResult(pgxmock.NewResult("DELETE", 2))
	mock.ExpectExec("INSERT INTO ip_pool_nodes (ip_pool_id, node_id) VALUES ($1, $2)").
		WithArgs(int64(3), int64(11)).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("INSERT INTO ip_pool_nodes (ip_pool_id, node_id) VALUES ($1, $2)").
		WithArgs(int64(3), int64(12)).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	repo := NewIPPoolRepository(mock)
	if err := repo.SetPoolNodes(context.Background(), 3, []int64{11, 12}); err != nil {
		t.Fatalf("SetPoolNodes: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestReleaseIPByVM(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE ips SET status='free', vm_id=NULL, updated_at=now() WHERE vm_id=$1").
		WithArgs(int64(9)).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	repo := NewIPPoolRepository(mock)
	if err := repo.ReleaseIPByVM(context.Background(), 9); err != nil {
		t.Fatalf("ReleaseIPByVM: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestGetPoolNodesReadsPveName 验证 GetPoolNodes 的 JOIN 查询按 nodeCols
// 列序扫描 pve_name 与 port 列（防列错位回归）：两个节点行分别携带
// pve_name 非空与空值。
func TestGetPoolNodesReadsPveName(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT n.id, zone_id, name, pve_name, host, port, api_user, api_token_secret, enabled, created_at FROM pve_nodes n JOIN ip_pool_nodes pn ON pn.node_id = n.id WHERE pn.ip_pool_id=$1 ORDER BY n.id").
		WithArgs(int64(3)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "zone_id", "name", "pve_name", "host", "port", "api_user", "api_token_secret", "enabled", "created_at"}).
			AddRow(int64(11), int64(1), "pve1", "aeoliancloud", "10.0.0.1", int32(8443), "root@pam!spark", "s1", true, testTime).
			AddRow(int64(12), int64(1), "pve2", "", "10.0.0.2", int32(8006), "root@pam!spark", "s2", false, testTime))

	repo := NewIPPoolRepository(mock)
	nodes, err := repo.GetPoolNodes(context.Background(), 3)
	if err != nil {
		t.Fatalf("GetPoolNodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("nodes = %+v, want 2", nodes)
	}
	if nodes[0].Name != "pve1" || nodes[0].PveName != "aeoliancloud" || nodes[0].Port != 8443 {
		t.Fatalf("nodes[0] = %+v, want name pve1, pve_name aeoliancloud, port 8443", nodes[0])
	}
	if nodes[1].Name != "pve2" || nodes[1].PveName != "" || nodes[1].Port != 8006 {
		t.Fatalf("nodes[1] = %+v, want name pve2, empty pve_name, port 8006", nodes[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestGetPoolNoRows(t *testing.T) {
	mock := newMockPool(t)
	mock.ExpectQuery("SELECT id, zone_id, name, network_cidr, gateway, dns, created_at FROM ip_pools WHERE id=$1").
		WithArgs(int64(404)).WillReturnError(pgx.ErrNoRows)

	repo := NewIPPoolRepository(mock)
	_, err := repo.GetPool(context.Background(), 404)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("err = %v, want pgx.ErrNoRows", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
