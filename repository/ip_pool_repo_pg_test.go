//go:build pg

// Integration tests for the IP allocation concurrency contract against a
// real PostgreSQL instance (task 9.1). They are excluded from the default
// build and run only with -tags=pg:
//
//	SPARK_TEST_DSN='postgres://spark:spark@127.0.0.1:5432/spark_test' \
//	  go test -tags=pg ./repository/ -count=1 -run TestPG
//
// The suite connects to the DSN from SPARK_TEST_DSN (default
// postgres://spark:spark@127.0.0.1:5432/spark_test), applies the embedded
// migrations and then exercises ClaimFreeIP under real concurrency: N
// independent transactions race for a pool with only two free addresses.
//
// Data hygiene: the tests DELETE from ips/ip_pool_nodes/ip_pools (in FK
// order; vms.ip_id has ON DELETE SET NULL, so leftover vms rows cannot
// block the cleanup) before every round. The business tables are shared
// with the e2e suite, which truncates everything at its own start/end, so
// a leftover zone row here is harmless.
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

// pgTestDSN returns the test database DSN, defaulting to the local spark
// test database when SPARK_TEST_DSN is unset.
func pgTestDSN() string {
	if dsn := os.Getenv("SPARK_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://spark:spark@127.0.0.1:5432/spark_test"
}

// pgTestCleanIPData removes every row of the IP tables in FK order: ips
// first (its vm_id references vms with ON DELETE SET NULL and vms.ip_id
// references ips with ON DELETE SET NULL, so the delete always succeeds),
// then the pool-node whitelist, then the pools themselves.
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

// TestPGConcurrentIPClaim is the real-PostgreSQL concurrency case of
// task 9.1: a /30 pool with exactly two free addresses (direct repository
// insert, deliberately bypassing the service-level gateway/broadcast
// exclusion — the pool row keeps the /30 shape but the two ip rows are what
// the test needs) is raced by 20 goroutines, each claiming inside its own
// transaction. Exactly two claims may succeed; the two winners must hold
// different addresses; every loser must fail with ErrAllocationRetry (lost
// the conditional-update race) or pgx.ErrNoRows (pool exhausted). The
// scenario runs three rounds with the IP tables cleaned in between, so a
// fluke cannot pass.
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

			// The /30 has two host addresses (10.0.0.1/10.0.0.2); the
			// gateway takes one of them, so the service-level expansion
			// would leave a single usable address. The repository is
			// inserted directly on purpose: it accepts an explicit address
			// list, giving the test exactly two free rows to race for.
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
					// Every worker claims inside its own transaction: the
					// SELECT-then-UPDATE pair must be atomic against the
					// other workers, which is exactly what the conditional
					// claim relies on (and what this test verifies for
					// real).
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
			// The two losers that lost the race on a still-free address
			// must have been told to retry; the pool must now be empty.
			if _, err := ipPoolRepo.AllocateFreeIP(ctx, p.ID, nil); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("claim after exhaustion = %v, want pgx.ErrNoRows", err)
			}
		})
	}

	// Leave the shared test database tidy: the IP tables are cleaned after
	// the last round too.
	pgTestCleanIPData(t, ctx, pool)
}
