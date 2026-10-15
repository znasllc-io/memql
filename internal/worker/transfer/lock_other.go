//go:build !darwin && !linux && !windows

package transfer

import "errors"

func LockLocal(string) (func(), error) {
	return nil, errors.New("durable backup locks are unavailable on this platform")
}
