//go:build !darwin && !linux

package modelcall

import "fmt"

func readMemoryHeadroom() (memoryHeadroom, error) {
	return memoryHeadroom{}, fmt.Errorf("local inference memory admission is unsupported on this platform")
}
