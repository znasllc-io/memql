//go:build !darwin && !linux

package tools

import "errors"

var pipelineAvailableDiskBytes = func(string) (uint64, error) {
	return 0, errors.New("build disk admission is supported only on macOS and Linux")
}
