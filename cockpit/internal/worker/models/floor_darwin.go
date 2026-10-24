package models

import (
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// platformFloor evaluates the macOS floor: Apple Silicon, 16 GB of
// unified memory, macOS 13 or newer.
//
// Every fact comes from a sysctl rather than a subprocess. This runs on
// the Register path of a LaunchAgent, and a floor check that forked
// `system_profiler` would put a multi-second stall in front of the
// worker's first connection on every start.
func platformFloor() FloorVerdict {
	return evalDarwinFloor(darwinProbe{
		AppleSilicon: darwinAppleSilicon,
		MemoryBytes:  darwinMemoryBytes,
		OSMajor:      darwinOSMajor,
	})
}

// darwinAppleSilicon reports whether this is Apple Silicon HARDWARE --
// which is not the same question as whether this is an arm64 process. An
// amd64 build under Rosetta on an M-series machine is still running on a
// GPU that can serve models, and `hw.optional.arm64` answers about the
// hardware where GOARCH answers about the binary.
func darwinAppleSilicon() (bool, error) {
	if v, err := unix.SysctlUint32("hw.optional.arm64"); err == nil {
		return v == 1, nil
	}
	// The sysctl is ABSENT on Intel Macs rather than zero, so failing to
	// read it is itself an answer. GOARCH backs that up for the arm64
	// build, where a missing sysctl would be a surprise not worth
	// compounding into a wrong verdict.
	return runtime.GOARCH == "arm64", nil
}

func darwinMemoryBytes() (uint64, error) {
	return unix.SysctlUint64("hw.memsize")
}

// darwinOSMajor reads the product version ("14.5", "10.15.7") and returns
// its major component.
func darwinOSMajor() (int, error) {
	v, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return 0, err
	}
	major, _, _ := strings.Cut(strings.TrimSpace(v), ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0, err
	}
	return n, nil
}
