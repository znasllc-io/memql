package models

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// platformFloor evaluates the Linux floor: x86_64 with a discrete GPU of
// at least 8 GB VRAM.
func platformFloor() FloorVerdict {
	return evalLinuxFloor(linuxProbe{Arch: goarch, GPUs: linuxGPUs})
}

// gpuProbeTimeout bounds the nvidia-smi call. A wedged driver must not
// hold up the worker's registration behind it.
const gpuProbeTimeout = 5 * time.Second

// linuxGPUs finds discrete GPUs and their VRAM.
//
// Two surfaces, because the two vendors expose it differently and neither
// is present on a machine without that vendor's driver: nvidia-smi for
// CUDA, and the amdgpu kernel driver's sysfs node for ROCm. INTEGRATED
// GRAPHICS ARE NOT REPORTED by either -- an iGPU shares system memory and
// has no mem_info_vram_total, so the floor's "discrete" requirement falls
// out of the probe rather than needing a separate test.
//
// Finding nothing returns an empty slice with no error: a machine with no
// GPU is the ordinary case across most of a fleet, not a fault.
func linuxGPUs() ([]GPU, error) {
	var out []GPU
	out = append(out, nvidiaGPUs()...)
	amd, err := amdGPUs()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// A sysfs read that failed for a reason other than absence is
		// worth surfacing only when nothing else was found -- otherwise
		// a working NVIDIA card would be discarded over an unreadable
		// AMD node.
		if len(out) == 0 {
			return nil, err
		}
	}
	return append(out, amd...), nil
}

// nvidiaGPUs asks nvidia-smi. Absent binary, non-zero exit and
// unparseable output all mean "no NVIDIA GPU this probe can vouch for".
func nvidiaGPUs() []GPU {
	bin, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), gpuProbeTimeout)
	defer cancel()
	raw, err := exec.CommandContext(ctx, bin,
		"--query-gpu=name,memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	var out []GPU
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		name, mib, ok := strings.Cut(line, ",")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(mib), 10, 64)
		if err != nil {
			// The card is there; its memory is not legible. Report it
			// with zero VRAM rather than dropping it, so the verdict
			// says "reported no VRAM figure" instead of "no GPU" --
			// different problems, different fixes.
			out = append(out, GPU{Name: strings.TrimSpace(name)})
			continue
		}
		out = append(out, GPU{Name: strings.TrimSpace(name), VRAMBytes: n * 1024 * 1024})
	}
	return out
}

// amdGPUs reads the amdgpu driver's VRAM node. mem_info_vram_total is in
// bytes and exists only for discrete cards.
func amdGPUs() ([]GPU, error) {
	cards, err := filepath.Glob("/sys/class/drm/card[0-9]*/device/mem_info_vram_total")
	if err != nil || len(cards) == 0 {
		return nil, nil
	}
	var out []GPU
	for _, path := range cards {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			continue
		}
		out = append(out, GPU{Name: amdCardName(filepath.Dir(path)), VRAMBytes: n})
	}
	return out, nil
}

// amdCardName gives the card a name an operator recognises. The kernel
// exposes ids rather than marketing names, so the honest answer is the
// device id -- better than "GPU 0", which tells them nothing about which
// slot to look in.
func amdCardName(deviceDir string) string {
	raw, err := os.ReadFile(filepath.Join(deviceDir, "device"))
	if err != nil {
		return "AMD GPU"
	}
	return "AMD GPU " + strings.TrimSpace(string(raw))
}
