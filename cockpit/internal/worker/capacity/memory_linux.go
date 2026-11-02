//go:build linux

package capacity

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// hostPressureFloor is the host memory below which nothing new may start, even on
// a card with room of its own.
const hostPressureFloor = uint64(1) << 30

// ReadMemory reads the pool a model loads into: an NVIDIA card's VRAM through
// nvidia-smi, an AMD card's through the amdgpu driver's sysfs nodes, and otherwise
// system memory from /proc/meminfo.
//
// A discrete GPU has its own pool, and comparing its weights to host RAM would
// wrongly refuse a 24 GiB GPU on a 16 GiB host. So host memory only decides
// Pressure there.
//
// SEVERAL CARDS ARE NEVER ADDED TOGETHER. Device assignment is the runtime's, and
// a model cannot be assumed to fit on a combined device that does not exist, so
// the answer is the card with the least free.
func ReadMemory(ctx context.Context) (Memory, error) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return Memory{}, err
	}
	total, available, ok := parseMeminfo(string(raw))
	if !ok {
		return Memory{}, errors.New("/proc/meminfo states no MemTotal or MemAvailable")
	}
	pressure := available < hostPressureFloor
	if total, free, ok := nvidiaMemory(ctx); ok {
		return Memory{Kind: KindVRAM, Total: total, Available: free, Pressure: pressure}, nil
	}
	if total, free, ok := amdMemory(); ok {
		return Memory{Kind: KindVRAM, Total: total, Available: free, Pressure: pressure}, nil
	}
	return Memory{Kind: KindSystem, Total: total, Available: available}, nil
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

// amdMemory answers for the card with the least free. A card it cannot read is
// skipped rather than taken for an empty one.
func amdMemory() (total, free uint64, ok bool) {
	cards, _ := filepath.Glob("/sys/class/drm/card[0-9]*/device/mem_info_vram_total")
	for _, path := range cards {
		t, readTotal := readUint(path)
		u, readUsed := readUint(filepath.Join(filepath.Dir(path), "mem_info_vram_used"))
		if !readTotal || !readUsed || t == 0 || u > t {
			continue
		}
		if !ok || t-u < free {
			total, free, ok = t, t-u, true
		}
	}
	return total, free, ok
}

func readUint(path string) (uint64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	return n, err == nil
}
