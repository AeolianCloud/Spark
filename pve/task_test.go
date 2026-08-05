package pve

import (
	"strings"
	"testing"
)

func TestParseUPID(t *testing.T) {
	tests := []struct {
		name    string
		upid    string
		want    UPID
		wantErr bool
	}{
		{
			name: "canonical",
			upid: "UPID:pve1:0000FD0F:01CA7A4A:5FAB1EC4:qmsnapshot:100:root@pam:",
			want: UPID{
				Raw:       "UPID:pve1:0000FD0F:01CA7A4A:5FAB1EC4:qmsnapshot:100:root@pam:",
				Node:      "pve1",
				PID:       0xFD0F,
				PStart:    0x1CA7A4A,
				StartTime: 0x5FAB1EC4,
				Type:      "qmsnapshot",
				ID:        "100",
				User:      "root@pam",
			},
		},
		{
			name: "example from task",
			upid: "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:qmcreate:100:root@pam:",
			want: UPID{
				Raw:       "UPID:pve1:00000E5B:01C9EC9E:5FAB1EC4:qmcreate:100:root@pam:",
				Node:      "pve1",
				PID:       3675,
				PStart:    30010526,
				StartTime: 1605050052,
				Type:      "qmcreate",
				ID:        "100",
				User:      "root@pam",
			},
		},
		{
			name: "lowercase hex",
			upid: "UPID:node2:0:0:0:vzdump:105:user@realm:",
			want: UPID{Raw: "UPID:node2:0:0:0:vzdump:105:user@realm:", Node: "node2", Type: "vzdump", ID: "105", User: "user@realm"},
		},
		{
			name:    "wrong prefix",
			upid:    "XPID:pve1:0:0:0:t:1:u:",
			wantErr: true,
		},
		{
			name:    "too few fields",
			upid:    "UPID:pve1:0000FD0F:01CA7A4A:5FAB1EC4:qmsnapshot:100",
			wantErr: true,
		},
		{
			name:    "non-hex pid",
			upid:    "UPID:pve1:zzzz:01CA7A4A:5FAB1EC4:qmsnapshot:100:root@pam:",
			wantErr: true,
		},
		{
			name:    "empty",
			upid:    "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseUPID(tt.upid)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseUPID(%q) = %+v, want error", tt.upid, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseUPID(%q): %v", tt.upid, err)
			}
			if got != tt.want {
				t.Fatalf("ParseUPID(%q) = %+v, want %+v", tt.upid, got, tt.want)
			}
		})
	}
}

func TestParseUPIDNodeExtraction(t *testing.T) {
	upid := "UPID:pve2:0000FD0F:01CA7A4A:5FAB1EC4:qmstart:100:root@pam:"
	parsed, err := ParseUPID(upid)
	if err != nil {
		t.Fatalf("ParseUPID: %v", err)
	}
	// WaitTask must prefer the node carried by the UPID over the caller's.
	if parsed.Node != "pve2" {
		t.Fatalf("node = %q, want pve2", parsed.Node)
	}
	if !strings.HasPrefix(parsed.Raw, "UPID:") {
		t.Fatalf("raw UPID mangled: %q", parsed.Raw)
	}
}
