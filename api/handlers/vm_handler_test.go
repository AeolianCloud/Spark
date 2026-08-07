package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

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
