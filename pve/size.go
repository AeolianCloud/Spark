package pve

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// PVE size 参数语义的大小后缀辅助函数。PVE 使用二进制单位（1G = 1024^3
// 字节，与 qemu 磁盘大小一致），并接受可选的 "+" 前缀，表示"在现有大小
// 之上累加"而非"绝对值"。服务层以整 GiB 为单位（vms.disk_gb），因此
// 绝对目标表示为 "10G" 字符串。

// sizeRe 匹配 PVE 的 size 模式：\d+(\.\d+)? 带可选的 K/M/G/T 后缀、可选
// 的前导 "+"，大小写不敏感。
var sizeRe = regexp.MustCompile(`(?i)^\+?(\d+(?:\.\d+)?)([KMGT])?$`)

// sizeUnits 将 PVE 大小后缀映射为二进制字节数。
var sizeUnits = map[string]int64{
	"K": 1 << 10,
	"M": 1 << 20,
	"G": 1 << 30,
	"T": 1 << 40,
}

// FormatSizeGB 为 PVE 的 size 参数渲染以整 GiB 计的绝对磁盘大小，
// 例如 10 -> "10G"。
func FormatSizeGB(gb int64) string {
	return fmt.Sprintf("%dG", gb)
}

// ParseSize 将 PVE 的 size 字符串转换为字节数，例如 "10G" -> 10737418240。
// 前导的 "+"（相对大小）会被接受并忽略；负大小会被拒绝。小数值截断为
// 整数字节。
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
