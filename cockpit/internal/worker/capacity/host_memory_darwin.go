//go:build darwin

package capacity

import (
	"encoding/binary"
	"errors"
	"golang.org/x/sys/unix"
)

func ReadSystemHeadroom() (SystemHeadroom, error) {
	names := []string{"hw.memsize", "hw.pagesize", "vm.page_free_count", "vm.page_purgeable_count", "vm.page_pageable_external_count", "kern.memorystatus_vm_pressure_level"}
	values := make([]uint64, len(names))
	for i, name := range names {
		raw, err := unix.SysctlRaw(name)
		if err != nil {
			return SystemHeadroom{}, err
		}
		switch len(raw) {
		case 4:
			values[i] = uint64(binary.NativeEndian.Uint32(raw))
		case 8:
			values[i] = binary.NativeEndian.Uint64(raw)
		default:
			return SystemHeadroom{}, errors.New("unexpected host memory counter width")
		}
	}
	total, page := values[0], values[1]
	if total == 0 || page == 0 {
		return SystemHeadroom{}, errors.New("empty host memory measurement")
	}
	// Free already includes speculative pages. Do not count anonymous inactive
	// or compressed pages as available without swapping another application out.
	var pages uint64
	for _, n := range values[2:5] {
		if n > ^uint64(0)-pages {
			return SystemHeadroom{}, errors.New("host page count overflow")
		}
		pages += n
	}
	if pages > ^uint64(0)/page {
		return SystemHeadroom{}, errors.New("host memory count overflow")
	}
	return SystemHeadroom{Total: total, Available: min(total, pages*page), Pressure: darwinPressure(values[5])}, nil
}

// The levels kern.memorystatus_vm_pressure_level reports: XNU answers the
// dispatch memory-pressure levels, which are bit values, not a scale.
const (
	darwinPressureNormal   = 1 // DISPATCH_MEMORYPRESSURE_NORMAL
	darwinPressureWarn     = 2 // DISPATCH_MEMORYPRESSURE_WARN
	darwinPressureCritical = 4 // DISPATCH_MEMORYPRESSURE_CRITICAL
)

// darwinPressure is whether the kernel's level alone refuses new work.
//
// ONLY CRITICAL REFUSES. A Mac in ordinary use reads warn for long stretches
// (compressed memory is how macOS uses RAM), and reading warn as pressure
// refused every pipeline step on such a machine. At warn the caller's own
// check decides: available memory against the step's need plus its reserve
// (checkPipelineHeadroom). Critical is the kernel saying it is about to kill
// processes, which no measured headroom outweighs.
//
// A level this code does not know is pressure: the kernel's answer is not
// understood, so the safe reading is that it said no.
func darwinPressure(level uint64) bool {
	switch level {
	case darwinPressureNormal, darwinPressureWarn:
		return false
	case darwinPressureCritical:
		return true
	}
	return true
}
