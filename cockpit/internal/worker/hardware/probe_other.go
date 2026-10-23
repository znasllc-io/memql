//go:build !darwin && !linux

package hardware

import "runtime"

// platformProbe: every other GOOS. Windows and the BSDs are not worker
// platforms for this cockpit, so nothing is probed and the inventory is
// the honest empty one -- which the engine reads as a machine that
// could tell it nothing, not as a machine with no memory.
func platformProbe() Probe {
	return Probe{
		GOOS:     runtime.GOOS,
		GOARCH:   runtime.GOARCH,
		CPUCores: runtime.NumCPU,
		Runtimes: detectRuntimes,
		Now:      now,
	}
}
