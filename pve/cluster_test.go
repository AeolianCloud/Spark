package pve

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// TestNextVMID 验证 GET /cluster/nextid 的请求形态以及 string->int 的
// 转换（PVE 以 JSON 字符串返回候选值）。
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

// TestNextVMIDNonInteger 呈现 PVE 返回非数字候选值的情况。
func TestNextVMIDNonInteger(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": "not-a-vmid"}`)
	})
	if _, err := c.NextVMID(context.Background()); err == nil {
		t.Fatal("NextVMID succeeded, want error for a non-integer vmid")
	}
}

// TestNextVMIDEmptyData 覆盖 null/空的 data 负载。
func TestNextVMIDEmptyData(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data": null}`)
	})
	if _, err := c.NextVMID(context.Background()); err == nil {
		t.Fatal("NextVMID succeeded, want error for a null payload")
	}
}
