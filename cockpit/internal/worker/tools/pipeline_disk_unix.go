//go:build darwin || linux

package tools

import (
	"errors"
	"math"
	"syscall"
)

// Bavail excludes blocks reserved for privileged users. A worker must not
// admit a build based on space its own operating-system account cannot use.
var pipelineAvailableDiskBytes = availablePipelineDiskBytes

func availablePipelineDiskBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	if stat.Bsize <= 0 || uint64(stat.Bavail) > math.MaxUint64/uint64(stat.Bsize) {
		return 0, errors.New("filesystem returned an invalid available-space measurement")
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}
