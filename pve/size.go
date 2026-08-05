package pve

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Size-suffix helpers for PVE's size parameter semantics. PVE uses binary
// units (1G = 1024^3 bytes, matching qemu disk sizes) and accepts an optional
// "+" prefix that means "add to the current size" instead of "absolute".
// The service layer deals in whole GiB (vms.disk_gb), so absolute targets are
// expressed as "10G" strings.

// sizeRe matches PVE's size pattern: \d+(\.\d+)? with an optional K/M/G/T
// suffix, optional leading "+", case-insensitive.
var sizeRe = regexp.MustCompile(`(?i)^\+?(\d+(?:\.\d+)?)([KMGT])?$`)

// sizeUnits maps PVE size suffixes to binary bytes.
var sizeUnits = map[string]int64{
	"K": 1 << 10,
	"M": 1 << 20,
	"G": 1 << 30,
	"T": 1 << 40,
}

// FormatSizeGB renders an absolute disk size in whole GiB for PVE's size
// parameter, e.g. 10 -> "10G".
func FormatSizeGB(gb int64) string {
	return fmt.Sprintf("%dG", gb)
}

// ParseSize converts a PVE size string to bytes, e.g. "10G" -> 10737418240.
// A leading "+" (relative size) is accepted and ignored; negative sizes are
// rejected. Fractional values are truncated to whole bytes.
func ParseSize(s string) (int64, error) {
	m := sizeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("pve: invalid size %q", s)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("pve: invalid size %q", s)
	}
	unit := int64(1)
	if m[2] != "" {
		unit = sizeUnits[strings.ToUpper(m[2])]
	}
	bytes := v * float64(unit)
	if bytes > float64(math.MaxInt64) {
		return 0, fmt.Errorf("pve: size %q overflows int64", s)
	}
	return int64(bytes), nil
}
