package handlers

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"spark/model"
	"spark/repository"
	"spark/service"
)

// TestMapVMServiceErrorImportKinds 锁定"导入已有 VM"新增错误类型的映射：
// vmid 不在节点 PVE 上 -> 404 vm_not_found_on_node；重复托管 -> 409
// vm_already_managed；而 zone/node 不存在的普通 404 仍保持 not_found，
// 不受新 Kind 影响。
func TestMapVMServiceErrorImportKinds(t *testing.T) {
	tests := []struct {
		name       string
		serr       *service.Error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "vmid absent on node maps to 404 vm_not_found_on_node",
			serr:       &service.Error{Kind: service.KindVMNotFoundOnNode, Message: "vm 100 not found on node \"pve-1\""},
			wantStatus: http.StatusNotFound,
			wantCode:   CodeVMNotFoundOnNode,
		},
		{
			name:       "duplicate managed vm maps to 409 vm_already_managed",
			serr:       &service.Error{Kind: service.KindVMAlreadyManaged, Message: "vm already managed: node 1 pve_vmid 100"},
			wantStatus: http.StatusConflict,
			wantCode:   CodeVMAlreadyManaged,
		},
		{
			name:       "missing zone keeps generic not_found",
			serr:       &service.Error{Kind: service.KindNotFound, Message: "zone 9 not found"},
			wantStatus: http.StatusNotFound,
			wantCode:   CodeNotFound,
		},
		{
			name:       "malformed vm id maps to 400 invalid_vm_id",
			serr:       &service.Error{Kind: service.KindInvalidVMRef, Message: "invalid external vm id \"ext-1\""},
			wantStatus: http.StatusBadRequest,
			wantCode:   CodeInvalidVMID,
		},
		{
			name:       "operation record write failure maps to 500 operation_log_failed",
			serr:       &service.Error{Kind: service.KindOperationLogFailed, Message: "start accepted but record failed"},
			wantStatus: http.StatusInternalServerError,
			wantCode:   CodeOperationLogFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiErr, ok := mapVMServiceError(tt.serr).(*APIError)
			if !ok {
				t.Fatalf("mapVMServiceError(%v) = %T, want *APIError", tt.serr, mapVMServiceError(tt.serr))
			}
			if apiErr.Status != tt.wantStatus {
				t.Errorf("status = %d, want %d", apiErr.Status, tt.wantStatus)
			}
			if apiErr.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", apiErr.Code, tt.wantCode)
			}
		})
	}
}

// TestVMResponseOmitsNilAssociations 验证导入的 VM（image_id/storage_type_id
// 为 nil）序列化时省略这两个字段；有值（普通创建）时正常输出 ——
// 契约要求导入响应中不出现无关的关联字段。
func TestVMResponseOmitsNilAssociations(t *testing.T) {
	imported := toVMResponse(&repository.VMWithIP{VM: model.VM{ID: 7, ImageID: nil, StorageTypeID: nil}}, "ready")
	raw, err := json.Marshal(imported)
	if err != nil {
		t.Fatalf("marshal imported vm: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["image_id"]; ok {
		t.Error("imported vm must omit image_id")
	}
	if _, ok := m["storage_type_id"]; ok {
		t.Error("imported vm must omit storage_type_id")
	}

	imgID, stID := int64(3), int64(5)
	created := toVMResponse(&repository.VMWithIP{VM: model.VM{ID: 8, ImageID: &imgID, StorageTypeID: &stID}}, "ready")
	raw, err = json.Marshal(created)
	if err != nil {
		t.Fatalf("marshal created vm: %v", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["image_id"] != float64(3) {
		t.Errorf("image_id = %v, want 3", m["image_id"])
	}
	if m["storage_type_id"] != float64(5) {
		t.Errorf("storage_type_id = %v, want 5", m["storage_type_id"])
	}
}

// TestVMListItemExternalSerialization 固定 external 条目的公开形态（设计
// D2）：id 为合成标识 ext-{nodeID}-{vmid}（字符串）、uuid/created_at 为
// 空字符串、source=external、规格来自 PVE 摘要；本地条目的 id 保持数字。
func TestVMListItemExternalSerialization(t *testing.T) {
	// 本地条目：id 保持数字。
	local := toVMListItem(&service.VMListItem{
		VM: repository.VMWithIP{VM: model.VM{ID: 7, UUID: "u-1", Name: "vm1", CPU: 2, MemMB: 2048, DiskGB: 10,
			Source: model.VMSourceSparkCreated, CreatedAt: time.Unix(100, 0)}},
		Status: "running",
	})
	raw, err := json.Marshal(local)
	if err != nil {
		t.Fatalf("marshal local item: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal local item: %v", err)
	}
	if m["id"] != float64(7) {
		t.Errorf("local id = %v, want numeric 7", m["id"])
	}
	if m["source"] != model.VMSourceSparkCreated || m["uuid"] != "u-1" {
		t.Errorf("local source/uuid = %v / %v", m["source"], m["uuid"])
	}

	// external 条目：合成 id、uuid/created_at 空、source=external。
	ext := toVMListItem(&service.VMListItem{
		VM: repository.VMWithIP{VM: model.VM{Name: "ext-vm", ZoneID: 1, NodeID: 3, PVEVmid: 200,
			CPU: 4, MemMB: 8192, DiskGB: 100, Source: model.VMSourceExternal}},
		Status:     "running",
		ExternalID: "ext-3-200",
	})
	raw, err = json.Marshal(ext)
	if err != nil {
		t.Fatalf("marshal external item: %v", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal external item: %v", err)
	}
	if m["id"] != "ext-3-200" {
		t.Errorf("external id = %v, want ext-3-200", m["id"])
	}
	if m["uuid"] != "" || m["created_at"] != "" {
		t.Errorf("external uuid/created_at = %v / %v, want empty strings", m["uuid"], m["created_at"])
	}
	if m["source"] != model.VMSourceExternal || m["name"] != "ext-vm" ||
		m["cpu"] != float64(4) || m["mem_mb"] != float64(8192) || m["disk_gb"] != float64(100) ||
		m["node_id"] != float64(3) || m["pve_vmid"] != float64(200) {
		t.Errorf("external fields = %v, want the PVE summary values", m)
	}
}
