//go:build darwin

package capacity

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
)

// probeMemory reads unified memory. Available is what vm_stat counts as free,
// inactive and speculative -- memory the system hands to a new allocation
// without paging anything out. ModelLimit is the GPU's wired ceiling: the
// iogpu.wired_limit_mb override when one is set, otherwise three quarters of
// the pool, the share the engine's UsableGigabytes assumes.
func probeMemory(ctx context.Context) (Memory, bool) {
	total, ok := sysctlUint(ctx, "hw.memsize")
	if !ok || total == 0 {
		return Memory{}, false
	}
	out, err := exec.CommandContext(ctx, "vm_stat").Output()
	if err != nil {
		return Memory{}, false
	}
	available, ok := parseVMStat(string(out))
	if !ok {
		return Memory{}, false
	}
	limit := total / 4 * 3
	if mb, ok := sysctlUint(ctx, "iogpu.wired_limit_mb"); ok && mb > 0 {
		limit = mb << 20
	}
	return Memory{Kind: KindUnified, Total: total, Available: available, ModelLimit: min(limit, total)}, true
}

func sysctlUint(ctx context.Context, name string) (uint64, bool) {
	out, err := exec.CommandContext(ctx, "sysctl", "-n", name).Output()
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	return n, err == nil
}
