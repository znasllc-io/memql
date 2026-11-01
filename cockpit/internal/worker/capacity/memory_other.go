//go:build !darwin && !linux

package capacity

import (
	"context"
	"errors"
)

// ReadMemory measures nothing on a platform this package does not probe, so
// the machine sends no report and is admitted on its slots alone.
func ReadMemory(context.Context) (Memory, error) {
	return Memory{}, errors.New("local inference memory admission is unsupported on this platform")
}
