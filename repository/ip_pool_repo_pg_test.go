//go:build pg

// 针对真实 PostgreSQL 实例的 IP 分配并发契约集成测试（任务 9.1）。
// 它们被排除在默认构建之外，仅通过 -tags=pg 运行：
//
//	SPARK_TEST_DSN='postgres://spark:spark@127.0.0.1:5432/spark_test' \
//	  go test -tags=pg ./repository/ -count=1 -run TestPG
//
// 测试套件从 SPARK_TEST_DSN（默认
// postgres://spark:spark@127.0.0.1:5432/spark_test）连接数据库，
// 应用内嵌 migration，然后在真实并发下演练 ClaimFreeIP：N 个独立
// 事务竞争一个只有两个空闲地址的池。
//
// 数据卫生：每轮测试前按 FK 顺序从 ips/ip_pool_nodes/ip_pools 中
// DELETE（vms.ip_id 为 ON DELETE SET NULL，因此遗留的 vms 行不会
// 阻塞清理）。业务表与 e2e 套件共享，后者会在自己的开始/结束时
// TRUNCATE 全部内容，因此此处遗留的区域行无害。
package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark/database"
	"spark/model"
)

// pgTestDSN 返回测试数据库 DSN；当 SPARK_TEST_DSN 未设置时默认
// 使用本地 spark 测试数据库。
func pgTestDSN() string {
	if dsn := os.Getenv("SPARK_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://spark:spark@127.0.0.1:5432/spark_test"
}

// pgTestCleanIPData 按 FK 顺序删除 IP 表的每一行：先删 ips（其 vm_id
// 引用 vms 且为 ON DELETE SET NULL，vms.ip_id 引用 ips 且为 ON DELETE
// SET NULL，因此删除总能成功），再删池-节点白名单，最后删池本身。
func pgTestCleanIPData(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, stmt := range []string{
		"DELETE FROM ips",
		"DELETE FROM ip_pool_nodes",
		"DELETE FROM ip_pools",
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("cleanup %q: %v", stmt, err)
		}
	}
}

// TestPGConcurrentIPClaim 是任务 9.1 的真实 PostgreSQL 并发用例：
// 一个恰好含两个空闲地址的 /30 池（直接仓库插入，刻意绕过服务层的
// 网关/广播地址排除——池行保留 /30 形态，但测试需要的是那两条 ip
// 行）被 20 个 goroutine 竞争，每个都在自己的事务内领取。恰好两次
// 领取可成功；两个获胜者必须持有不同的地址；每个败者必须以
// ErrAllocationRetry（输掉条件式更新竞争）或 pgx.ErrNoRows（池耗尽）
// 失败。该场景运行三轮，轮次之间清理 IP 表，因此侥幸无法通过。
func TestPGConcurrentIPClaim(t *testing.T) {
	ctx := context.Background()

	pool, err := database.New(ctx, pgTestDSN())
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	defer pool.Close()

	if err := database.Migrate(ctx, pool, database.MigrationFS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	zoneRepo := NewZoneRepository(pool)
	zone, err := zoneRepo.CreateZone(ctx, "pg-test-zone")
	if err != nil {
		t.Fatalf("create test zone: %v", err)
	}

	ipPoolRepo := NewIPPoolRepository(pool)

	const (
		workers     = 20
		freeIPs     = 2
		networkCIDR = "10.0.0.0/30"
		gateway     = "10.0.0.1"
	)

	for round := 1; round <= 3; round++ {
		t.Run(fmt.Sprintf("round-%d", round), func(t *testing.T) {
			pgTestCleanIPData(t, ctx, pool)

			// /30 有两个主机地址（10.0.0.1/10.0.0.2）；网关占用其中之一，
			// 因此服务层的展开只会留下一个可用地址。这里刻意直接调用
			// 仓库插入：它接受显式地址列表，给测试恰好两条可竞争的空闲行。
			p, err := ipPoolRepo.CreatePoolWithIPs(ctx, model.IPPool{
				ZoneID: zone.ID, Name: fmt.Sprintf("c30-round%d", round),
				NetworkCIDR: networkCIDR, Gateway: gateway, DNS: "1.1.1.1",
			}, []model.IP{{IP: "10.0.0.2"}, {IP: "10.0.0.3"}})
			if err != nil {
				t.Fatalf("create pool: %v", err)
			}

			results := make([]model.IP, 0, workers)
			failures := make([]error, 0, workers)
			var mu sync.Mutex
			var wg sync.WaitGroup

			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					// 每个 worker 都在自己的事务内领取：SELECT 后紧跟
					// UPDATE 的组合必须对其他 worker 具有原子性，这正是
					// 条件式领取所依赖的（也是本测试要真实验证的）。
					tx, err := pool.Begin(ctx)
					if err != nil {
						mu.Lock()
						failures = append(failures, fmt.Errorf("begin: %w", err))
						mu.Unlock()
						return
					}
					defer func() { _ = tx.Rollback(ctx) }()

					ip, err := ipPoolRepo.ClaimFreeIP(ctx, tx, p.ID, nil)
					if err == nil {
						if err := tx.Commit(ctx); err != nil {
							mu.Lock()
							failures = append(failures, fmt.Errorf("commit: %w", err))
							mu.Unlock()
							return
						}
						mu.Lock()
						results = append(results, ip)
						mu.Unlock()
						return
					}
					mu.Lock()
					failures = append(failures, err)
					mu.Unlock()
				}()
			}
			wg.Wait()

			if len(results) != freeIPs {
				t.Fatalf("successful claims = %d, want exactly %d (pool had %d free addresses)",
					len(results), freeIPs, freeIPs)
			}
			if results[0].IP == results[1].IP {
				t.Fatalf("two claims got the same address %s", results[0].IP)
			}
			for i, f := range failures {
				if !errors.Is(f, ErrAllocationRetry) && !errors.Is(f, pgx.ErrNoRows) {
					t.Fatalf("failure %d = %v, want ErrAllocationRetry or pgx.ErrNoRows", i, f)
				}
			}
			// 在仍空闲的地址上输掉竞争的两个败者必须被告知重试；
			// 此时池内必须已经为空。
			if _, err := ipPoolRepo.AllocateFreeIP(ctx, p.ID, nil); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("claim after exhaustion = %v, want pgx.ErrNoRows", err)
			}
		})
	}

	// 保持共享测试数据库整洁：最后一轮结束后同样清理 IP 表。
	pgTestCleanIPData(t, ctx, pool)
}
