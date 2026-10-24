//go:build !darwin && !linux

package hardware

// modelVolumeFree: no supported model volume on this platform, so the
// figure is absent rather than a number from the wrong disk.
func modelVolumeFree() uint64 { return 0 }
