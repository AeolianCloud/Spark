package pve

import "testing"

func TestFormatSizeGB(t *testing.T) {
	tests := []struct {
		gb   int64
		want string
	}{
		{0, "0G"},
		{10, "10G"},
		{1, "1G"},
		{-5, "-5G"},
	}
	for _, tt := range tests {
		if got := FormatSizeGB(tt.gb); got != tt.want {
			t.Errorf("FormatSizeGB(%d) = %q, want %q", tt.gb, got, tt.want)
		}
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"10G", 10 * 1 << 30, false},
		{"10g", 10 * 1 << 30, false},
		{"512M", 512 * 1 << 20, false},
		{"1.5T", 1649267441664, false},
		{"+10G", 10 * 1 << 30, false},
		{"1024", 1024, false},
		{"1.5G", 1610612736, false},
		{" 10G ", 10 * 1 << 30, false},
		{"", 0, true},
		{"10GB", 0, true},
		{"abc", 0, true},
		{"-10G", 0, true},
		{"10.5.5G", 0, true},
	}
	for _, tt := range tests {
		got, err := ParseSize(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseSize(%q) = %d, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSize(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseSize(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// TestParseSizeRoundTrip 保证 FormatSizeGB 与 ParseSize 对服务层产出的
// 大小（整 GiB）保持一致性。
func TestParseSizeRoundTrip(t *testing.T) {
	for _, gb := range []int64{1, 4, 10, 32, 100} {
		bytes, err := ParseSize(FormatSizeGB(gb))
		if err != nil {
			t.Fatalf("ParseSize(FormatSizeGB(%d)): %v", gb, err)
		}
		if want := gb * 1 << 30; bytes != want {
			t.Errorf("round trip %dG = %d bytes, want %d", gb, bytes, want)
		}
	}
}
