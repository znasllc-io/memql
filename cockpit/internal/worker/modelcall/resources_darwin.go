//go:build darwin

package modelcall

import (
	"encoding/binary"
	"fmt"
	"golang.org/x/sys/unix"
)

func readMemoryHeadroom() (memoryHeadroom, error) {
	total, err := kernelCount("hw.memsize")
	if err != nil {
		return memoryHeadroom{}, err
	}
	page, err := kernelCount("hw.pagesize")
	if err != nil {
		return memoryHeadroom{}, err
	}
	free, err := kernelCount("vm.page_free_count")
	if err != nil {
		return memoryHeadroom{}, err
	}
	// Purgeable pages can be reclaimed without swapping application memory.
	// Do not count all inactive/compressed pages as free, or count speculative
	// pages twice: XNU's free count already includes the speculative queue.
	purgeable, err := kernelCount("vm.page_purgeable_count")
	if err != nil {
		return memoryHeadroom{}, err
	}
	// File-backed pageable pages are reclaimable by dropping the file cache,
	// unlike anonymous inactive pages which would require compression/swap.
	fileBacked, err := kernelCount("vm.page_pageable_external_count")
	if err != nil {
		return memoryHeadroom{}, err
	}
	pressure, err := kernelCount("kern.memorystatus_vm_pressure_level")
	if err != nil {
		return memoryHeadroom{}, err
	}
	return memoryHeadroom{Total: total, Available: min(total, (uint64(free)+uint64(purgeable)+uint64(fileBacked))*uint64(page)), Pressure: pressure != 1}, nil
}

// XNU exposes counts as both unsigned int and unsigned long depending on
// the key/OS release. Read the actual width instead of truncating a counter.
func kernelCount(name string) (uint64, error) {
	raw, err := unix.SysctlRaw(name)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	switch len(raw) {
	case 4:
		return uint64(binary.NativeEndian.Uint32(raw)), nil
	case 8:
		return binary.NativeEndian.Uint64(raw), nil
	}
	return 0, fmt.Errorf("%s: unexpected counter width %d", name, len(raw))
}
