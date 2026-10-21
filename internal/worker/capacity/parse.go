package capacity

import (
	"strconv"
	"strings"
)

// parseMeminfo reads MemTotal and MemAvailable, in kB, from /proc/meminfo.
func parseMeminfo(raw string) (total, available uint64, ok bool) {
	var haveTotal, haveAvailable bool
	for _, line := range strings.Split(raw, "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(key) {
		case "MemTotal":
			total, haveTotal = n*1024, true
		case "MemAvailable":
			available, haveAvailable = n*1024, true
		}
	}
	return total, available, haveTotal && haveAvailable && total > 0
}

// parseNvidiaSMI sums `memory.total, memory.free` lines, in MiB.
func parseNvidiaSMI(out string) (total, free uint64, ok bool) {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		t, f, found := strings.Cut(line, ",")
		if !found {
			return 0, 0, false
		}
		tn, err1 := strconv.ParseUint(strings.TrimSpace(t), 10, 64)
		fn, err2 := strconv.ParseUint(strings.TrimSpace(f), 10, 64)
		if err1 != nil || err2 != nil {
			return 0, 0, false
		}
		total += tn << 20
		free += fn << 20
	}
	return total, free, total > 0 && free <= total
}
