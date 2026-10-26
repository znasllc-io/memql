//go:build !darwin && !linux

package capacity

import "errors"

func ReadSystemHeadroom() (SystemHeadroom, error) {
	return SystemHeadroom{}, errors.New("host memory admission is supported only on macOS and Linux")
}
