//go:build darwin

package capacity

import (
	"context"
	"encoding/binary"
	"fmt"

	"golang.org/x/sys/unix"
)

// ReadMemory reads unified memory. Available is what comes back without swapping
// anyone else's memory out (darwinAvailable). ModelLimit is the GPU's wired
// ceiling: the iogpu.wired_limit_mb override when one is set, otherwise three
// quarters of the pool, the share the engine's UsableGigabytes assumes. Pressure
// is any level the kernel reports other than normal.
func ReadMemory(context.Context) (Memory, error) {
	total, err := kernelCount("hw.memsize")
	if err != nil {
		return Memory{}, err
	}
	page, err := kernelCount("hw.pagesize")
	if err != nil {
		return Memory{}, err
	}
	free, err := kernelCount("vm.page_free_count")
	if err != nil {
		return Memory{}, err
	}
	purgeable, err := kernelCount("vm.page_purgeable_count")
	if err != nil {
		return Memory{}, err
	}
	fileBacked, err := kernelCount("vm.page_pageable_external_count")
	if err != nil {
		return Memory{}, err
	}
	pressure, err := kernelCount("kern.memorystatus_vm_pressure_level")
	if err != nil {
		return Memory{}, err
	}
	limit := total / 4 * 3
	if mb, err := kernelCount("iogpu.wired_limit_mb"); err == nil && mb > 0 {
		limit = mb << 20
	}
	return Memory{
		Kind:       KindUnified,
		Total:      total,
		Available:  darwinAvailable(free, purgeable, fileBacked, page, total),
		ModelLimit: min(limit, total),
		Pressure:   pressure != 1,
	}, nil
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
