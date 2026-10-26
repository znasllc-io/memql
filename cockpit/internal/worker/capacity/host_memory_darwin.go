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
	return SystemHeadroom{Total: total, Available: min(total, pages*page), Pressure: values[5] != 1}, nil
}
