package api

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark/config"
	"spark/crypto"
)

// testRouterCipher builds a cipher from a deterministic 32-byte key.
func testRouterCipher(t *testing.T) *crypto.Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	cfg := config.Default()
	cfg.Crypto.EncryptionKey = base64.StdEncoding.EncodeToString(key)
	c, err := crypto.NewCipher(cfg)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

// TestRouterRegistersAllRoutes builds the full router with a lazy pool (no
// connection is ever opened) and asserts the complete route table. gin
// panics on conflicting route registrations, so merely building the router
// is the conflict check; the assertions pin down the expected paths,
// including the batch-7 /vms group.
func TestRouterRegistersAllRoutes(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://user:pass@127.0.0.1:1/db")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	r := NewRouter(pool, testRouterCipher(t))

	want := []string{
		"GET /healthz",
		"POST /zones",
		"GET /zones",
		"POST /zones/:zone_id/nodes",
		"GET /zones/:zone_id/nodes",
		"PUT /nodes/:id",
		"POST /ip-pools",
		"GET /ip-pools",
		"PUT /ip-pools/:id/nodes",
		"GET /ip-pools/:id/nodes",
		"POST /storage-types",
		"GET /storage-types",
		"GET /storage-types/:id",
		"PUT /storage-types/:id",
		"DELETE /storage-types/:id",
		"POST /images",
		"GET /images",
		"POST /vms",
		"GET /vms",
		"GET /vms/:id",
		"POST /vms/:id/start",
		"POST /vms/:id/stop",
		"POST /vms/:id/restart",
		"POST /vms/:id/resize",
		"POST /vms/:id/destroy",
	}
	got := make(map[string]bool)
	for _, route := range r.Routes() {
		got[route.Method+" "+route.Path] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("route %q is not registered", w)
		}
	}
}
