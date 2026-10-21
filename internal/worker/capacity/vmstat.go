package capacity

import (
	"strconv"
	"strings"
)

// parseVMStat reads macOS vm_stat output: the page size from its header, and
// the free, inactive and speculative page counts.
func parseVMStat(out string) (uint64, bool) {
	lines := strings.Split(out, "\n")
	if len(lines) == 0 {
		return 0, false
	}
	page := uint64(0)
	if _, rest, ok := strings.Cut(lines[0], "page size of "); ok {
		if n, err := strconv.ParseUint(strings.Fields(rest)[0], 10, 64); err == nil {
			page = n
		}
	}
	if page == 0 {
		return 0, false
	}
	var pages uint64
	seen := 0
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Pages free", "Pages inactive", "Pages speculative":
			n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(value), "."), 10, 64)
			if err != nil {
				return 0, false
			}
			pages += n
			seen++
		}
	}
	if seen == 0 {
		return 0, false
	}
	return pages * page, true
}
