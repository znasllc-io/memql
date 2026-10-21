//go:build !darwin && !linux

package modelcall

import (
	"context"
	"errors"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"time"
)

type inferenceLease struct{}

func inferenceCapacityRoot() (string, error) {
	return "", errors.New("local inference admission is unsupported on this platform")
}
func acquireInferenceLease(context.Context, string, time.Duration) (*inferenceLease, error) {
	return nil, errors.New("local inference admission is unsupported on this platform")
}
func (*inferenceLease) close() {}
func (*inferenceLease) readOwned() (map[string]models.Info, error) {
	return nil, errors.New("local inference admission is unsupported on this platform")
}
func (*inferenceLease) saveOwned(map[string]models.Info) error {
	return errors.New("local inference admission is unsupported on this platform")
}
