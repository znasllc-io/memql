package imageverify

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Both the fixed systemd envelope and the delegated child must be measured.
// CPU and RAM apply to the helper, builder, exporter and all RUN descendants
// together, rather than to individual build operations that may run in parallel.
func checkCgroupLimits(values map[string]string, l Limits) error {
	cpu := strings.Fields(values["cpu.max"])
	if len(cpu) != 2 {
		return errors.New("CPU limit is missing")
	}
	quota, e1 := strconv.ParseUint(cpu[0], 10, 64)
	period, e2 := strconv.ParseUint(cpu[1], 10, 64)
	if e1 != nil || e2 != nil || quota == 0 || period == 0 || period > 1000000 || quota > uint64(l.CPUMilli)*period/1000 {
		return errors.New("CPU limit is not enforced")
	}
	for key, max := range map[string]uint64{"memory.max": uint64(l.MemoryMiB) << 20, "memory.swap.max": 0, "pids.max": 512} {
		n, e := strconv.ParseUint(strings.TrimSpace(values[key]), 10, 64)
		if e != nil || n > max || (n == 0 && key != "memory.swap.max") {
			return fmt.Errorf("%s is not enforced", key)
		}
	}
	return nil
}

func parseUnifiedCgroup(data string) (string, error) {
	lines := strings.Split(strings.TrimSpace(data), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "0::/") {
		return "", errors.New("a unified cgroup v2 hierarchy is required")
	}
	return strings.TrimPrefix(lines[0], "0::"), nil
}
