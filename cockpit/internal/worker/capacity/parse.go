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

// parseNvidiaSMI reads `memory.total, memory.free` lines, in MiB, and answers for
// the card with the least free. See ReadMemory for why cards are not summed.
func parseNvidiaSMI(out string) (total, free uint64, ok bool) {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		t, f, found := strings.Cut(line, ",")
		if !found {
			return 0, 0, false
		}
		tn, err1 := strconv.ParseUint(strings.TrimSpace(t), 10, 64)
		fn, err2 := strconv.ParseUint(strings.TrimSpace(f), 10, 64)
		if err1 != nil || err2 != nil || tn == 0 || fn > tn {
			return 0, 0, false
		}
		if !ok || fn<<20 < free {
			total, free, ok = tn<<20, fn<<20, true
		}
	}
	return total, free, ok
}

// darwinAvailable is what macOS hands a new allocation without swapping anyone
// else's memory out, from its page counts.
//
// Purgeable pages are given up on request. File-backed pages come back by dropping
// the file cache, and speculative pages are among them: the pageable internal and
// external counts sum to active, inactive and speculative, measured on macOS 26.
//
// INACTIVE PAGES ARE NOT COUNTED, which is the difference from vm_stat's reading of
// free, inactive and speculative. Most of the inactive queue can be anonymous
// memory that a new allocation gets only by compressing or swapping it. On one Mac
// at one instant the two read 21.6 GB and 33.3 GB (memql#6414), and the gate has
// always used this one.
func darwinAvailable(free, purgeable, fileBacked, pageSize, total uint64) uint64 {
	return min(total, (free+purgeable+fileBacked)*pageSize)
}
