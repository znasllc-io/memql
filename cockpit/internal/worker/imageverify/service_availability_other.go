//go:build !linux

package imageverify

import (
	"context"
	"errors"
)

func QualifiedServiceAvailable(context.Context) bool { return false }
func QualifiedService(context.Context, ServiceConfig) error {
	return errors.New("image verification requires its qualified Linux service")
}
