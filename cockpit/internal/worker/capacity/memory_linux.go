//go:build linux

package capacity

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// probeMemory reads the pool a model loads into: an NVIDIA card's VRAM
// through nvidia-smi, an AMD card's through the amdgpu driver's sysfs nodes,
// and otherwise system memory from /proc/meminfo. Several cards are summed:
// the runtime splits a model across them.
func probeMemory(ctx context.Context) (Memory, bool) {
	if total, free, ok := nvidiaMemory(ctx); ok {
		return Memory{Kind: KindVRAM, Total: total, Available: free}, true
	}
	if total, free, ok := amdMemory(); ok {
		return Memory{Kind: KindVRAM, Total: total, Available: free}, true
	}
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return Memory{}, false
	}
	total, available, ok := parseMeminfo(string(raw))
	if !ok {
		return Memory{}, false
	}
	return Memory{Kind: KindSystem, Total: total, Available: available}, true
}

func nvidiaMemory(ctx context.Context) (uint64, uint64, bool) {
	bin, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return 0, 0, false
	}
	out, err := exec.CommandContext(ctx, bin, "--query-gpu=memory.total,memory.free", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, 0, false
	}
	return parseNvidiaSMI(string(out))
}

func amdMemory() (uint64, uint64, bool) {
	cards, err := filepath.Glob("/sys/class/drm/card[0-9]*/device/mem_info_vram_total")
	if err != nil || len(cards) == 0 {
		return 0, 0, false
	}
	var total, used uint64
	for _, path := range cards {
		t, ok := readUint(path)
		if !ok {
			return 0, 0, false
		}
		u, ok := readUint(filepath.Join(filepath.Dir(path), "mem_info_vram_used"))
		if !ok {
			return 0, 0, false
		}
		total += t
		used += u
	}
	if total == 0 || used > total {
		return 0, 0, false
	}
	return total, total - used, true
}

func readUint(path string) (uint64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	return n, err == nil
}
