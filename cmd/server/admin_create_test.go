package main

import (
	"strings"
	"testing"
)

// TestValidateAdminCreate 覆盖管理员创建参数校验：username 必填（空/纯
// 空白）、password 必填、密码超过 bcrypt 72 字节上限被拒绝、恰好 72 字节
// 通过（bcrypt 上限边界）。错误消息不得携带密码内容（明文不落日志）。
func TestValidateAdminCreate(t *testing.T) {
	tests := []struct {
		name     string
		username string
		password string
		wantErr  bool
	}{
		{"合法参数", "root", "s3cret-password", false},
		{"username 缺失", "", "s3cret", true},
		{"username 纯空白", "   ", "s3cret", true},
		{"password 缺失", "root", "", true},
		{"password 超过 72 字节", "root", strings.Repeat("x", 73), true},
		{"password 恰好 72 字节", "root", strings.Repeat("x", 72), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// runAdminCreate 在调用 validate 前先 TrimSpace 裁剪用户名，
			// 测试按同样约定建模（纯空白用户名等效于空）。
			err := validateAdminCreate(strings.TrimSpace(tt.username), tt.password)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateAdminCreate(%q, <redacted>) = %v, wantErr %v", tt.username, err, tt.wantErr)
			}
			if err != nil && tt.password != "" && strings.Contains(err.Error(), tt.password) {
				t.Fatalf("error message %q leaks the password", err)
			}
		})
	}
}
