package capacity

// SystemHeadroom describes RAM available to host processes, never GPU VRAM.
// Admission is a fresh conservative measurement, not an OS quota.
//
// Pressure is the operating system's own word that new work must wait,
// whatever Available says. On macOS only the kernel's critical level sets it
// (darwinPressure); its warn level leaves the decision to Available. Linux
// reports no level, so Available alone decides there.
type SystemHeadroom struct {
	Total, Available uint64
	Pressure         bool
}
