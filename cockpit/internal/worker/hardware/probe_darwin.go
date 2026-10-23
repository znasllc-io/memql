//go:build darwin

package hardware

import (
	"runtime"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
)

// platformProbe answers the macOS facts.
//
// Every one comes from a sysctl or a statfs rather than a subprocess,
// which is the same choice the floor makes and for the same reason: this
// runs on the Register path of a LaunchAgent, and a scan that forked
// `system_profiler` would put a multi-second stall in front of the
// worker's first connection on every start. `system_profiler
// SPHardwareDataType` would give a prettier chip string; it takes
// seconds and it is not worth them.
func platformProbe() Probe {
	return Probe{
		GOOS:        "darwin",
		GOARCH:      runtime.GOARCH,
		Chip:        darwinChip,
		MemoryBytes: func() (uint64, error) { return unix.SysctlUint64("hw.memsize") },
		OSVersion:   darwinOSVersion,
		CPUCores:    darwinCPUCores,
		GPUs:        models.DetectGPUs,
		Backend:     models.GPUBackend,
		DiskFree:    modelVolumeFree,
		Runtimes:    detectRuntimes,
		Now:         now,
	}
}

// darwinChip reads the CPU brand string, which on Apple Silicon is the
// marketing name ("Apple M3 Max") and on Intel is the Intel one. Both
// are what the person would say if asked what machine this is.
func darwinChip() (string, error) { return unix.Sysctl("machdep.cpu.brand_string") }

// darwinOSVersion renders the product version the way a person says it.
// "macOS 15.1" rather than "15.1": the string travels to a fleet page
// beside Linux entries that name their distribution, and a bare number
// there is ambiguous.
func darwinOSVersion() (string, error) {
	v, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return "", err
	}
	return "macOS " + strings.TrimSpace(v), nil
}

// darwinCPUCores reports PHYSICAL cores (hw.physicalcpu), not logical
// ones. On Apple Silicon they are the same number; on Intel they are
// not, and the question this answers -- how much machine is this --
// is not helped by counting hyperthreads.
func darwinCPUCores() int {
	if v, err := unix.SysctlUint32("hw.physicalcpu"); err == nil && v > 0 {
		return int(v)
	}
	return runtime.NumCPU()
}
