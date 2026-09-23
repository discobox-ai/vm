// Package units parses the byte sizes specs and flags are written in.
package units

import (
	"fmt"
	"strconv"
	"strings"
)

var suffixes = []struct {
	suffix string
	scale  uint64
}{
	{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
	{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3},
	{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10},
	{"B", 1},
}

// ParseBytes reads "64GiB", "8G", "512MiB", or a plain byte count. Single
// letters are binary, as they are for every VM tool people already use.
func ParseBytes(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	for _, u := range suffixes {
		if number, ok := strings.CutSuffix(s, u.suffix); ok {
			value, err := strconv.ParseFloat(strings.TrimSpace(number), 64)
			if err != nil || value < 0 {
				return 0, fmt.Errorf("invalid size %q", s)
			}
			return uint64(value * float64(u.scale)), nil
		}
	}
	value, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q (use a suffix such as GiB)", s)
	}
	return value, nil
}

// FormatBytes renders a size the way ParseBytes reads it.
func FormatBytes(n uint64) string {
	for _, u := range suffixes[:4] {
		if n >= u.scale && n%u.scale == 0 {
			return fmt.Sprintf("%d%s", n/u.scale, u.suffix)
		}
	}
	return strconv.FormatUint(n, 10)
}
