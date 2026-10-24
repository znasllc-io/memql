//go:build !darwin && !linux

package capacity

import "context"

// probeMemory measures nothing on a platform this package does not probe, so
// the machine sends no report and is admitted on its slots alone.
func probeMemory(context.Context) (Memory, bool) { return Memory{}, false }
