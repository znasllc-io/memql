//go:build linux

package modelcall

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func readMemoryHeadroom() (memoryHeadroom, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return memoryHeadroom{}, err
	}
	defer f.Close()
	var out memoryHeadroom
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		var name, unit string
		var kb uint64
		if _, err := fmt.Sscan(scan.Text(), &name, &kb, &unit); err != nil || unit != "kB" {
			continue
		}
		switch strings.TrimSuffix(name, ":") {
		case "MemTotal":
			out.Total = kb * 1024
		case "MemAvailable":
			out.Available = kb * 1024
		}
	}
	if err := scan.Err(); err != nil {
		return out, err
	}
	if out.Total == 0 {
		return out, fmt.Errorf("MemTotal is absent")
	}
	// A discrete GPU has its own pool; comparing its weights to host RAM
	// would wrongly refuse a 24 GiB GPU on a 16 GiB host. Keep host pressure
	// protection while admitting against the GPU's actual free memory.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if raw, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=memory.total,memory.free", "--format=csv,noheader,nounits").Output(); err == nil {
		if gpu, ok := nvidiaHeadroom(string(raw)); ok {
			gpu.Pressure = out.Available < gib
			gpu.DedicatedGPU = true
			return gpu, nil
		}
	}
	// AMD's amdgpu driver exports the same facts without a vendor process.
	paths, _ := filepath.Glob("/sys/class/drm/card*/device/mem_info_vram_total")
	var gpu memoryHeadroom
	for _, path := range paths {
		totalRaw, totalErr := os.ReadFile(path)
		usedRaw, usedErr := os.ReadFile(filepath.Join(filepath.Dir(path), "mem_info_vram_used"))
		total, a := strconv.ParseUint(strings.TrimSpace(string(totalRaw)), 10, 64)
		used, b := strconv.ParseUint(strings.TrimSpace(string(usedRaw)), 10, 64)
		if totalErr != nil || usedErr != nil || a != nil || b != nil || total == 0 || used > total {
			continue
		}
		available := total - used
		if gpu.Total == 0 || available < gpu.Available {
			gpu = memoryHeadroom{Total: total, Available: available, Pressure: out.Available < gib}
		}
	}
	if gpu.Total != 0 {
		gpu.DedicatedGPU = true
		return gpu, nil
	}
	return out, nil
}

func nvidiaHeadroom(raw string) (memoryHeadroom, bool) {
	var out memoryHeadroom
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		parts := strings.Split(line, ",")
		if len(parts) != 2 {
			return memoryHeadroom{}, false
		}
		total, a := strconv.ParseUint(strings.TrimSpace(parts[0]), 10, 64)
		free, b := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
		if a != nil || b != nil || total == 0 || free > total {
			return memoryHeadroom{}, false
		}
		// Device assignment is runtime-owned. Never add capacities together
		// and pretend a model can fit on a nonexistent combined device.
		if out.Total == 0 || free*(1<<20) < out.Available {
			out = memoryHeadroom{Total: total * (1 << 20), Available: free * (1 << 20)}
		}
	}
	return out, out.Total != 0
}
