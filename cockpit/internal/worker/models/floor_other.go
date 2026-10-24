//go:build !darwin && !linux

package models

import "runtime"

// platformFloor: every other GOOS. Windows and the BSDs are not worker
// platforms for this cockpit, so there is no floor to fail -- there is no
// floor at all, which the verdict says in as many words.
func platformFloor() FloorVerdict { return evalUnsupportedFloor(runtime.GOOS) }
