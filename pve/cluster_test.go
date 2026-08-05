package pve

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// TestNextVMID verifies the request shape and the string->int conversion of
// GET /cluster/nextid (PVE returns the candidate as a JSON string).
func TestNextVMID(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/cluster/nextid" {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"data": "100"}`)
	})
	vmid, err := c.NextVMID(context.Background())
	if err != nil {
		t.Fatalf("NextVMID: %v", err)
	}
	if vmid != 100 {
		t.Fatalf("vmid = %d, want 100", vmid)
	}
}

// TestNextVMIDNonInteger surfaces PVE answering a non-numeric candidate.
func TestNextVMIDNonInteger(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": "not-a-vmid"}`)
	})
	if _, err := c.NextVMID(context.Background()); err == nil {
		t.Fatal("NextVMID succeeded, want error for a non-integer vmid")
	}
}

// TestNextVMIDEmptyData covers a null/empty data payload.
func TestNextVMIDEmptyData(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": null}`)
	})
	if _, err := c.NextVMID(context.Background()); err == nil {
		t.Fatal("NextVMID succeeded, want error for a null payload")
	}
}
