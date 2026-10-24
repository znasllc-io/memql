//go:build linux

package tools

import (
	"fmt"
	"math"
)

func shellMemoryLimitKiB(megabytes int) (int, error) {
	if megabytes <= 0 {
		return 0, nil
	}
	if megabytes > math.MaxInt/1024 {
		return 0, fmt.Errorf("shell memory limit is too large")
	}
	return megabytes * 1024, nil
}
