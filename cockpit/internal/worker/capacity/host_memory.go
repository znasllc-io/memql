package capacity

// SystemHeadroom describes RAM available to host processes, never GPU VRAM.
// Admission is a fresh conservative measurement, not an OS quota.
type SystemHeadroom struct {
	Total, Available uint64
	Pressure         bool
}
