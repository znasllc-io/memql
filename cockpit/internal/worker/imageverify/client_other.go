//go:build !linux

package imageverify

import "context"

func Available(context.Context) bool { return false }
func Run(context.Context, string, string, Request, Limits, Persist, Emit) error {
	return ErrUnavailable
}
func Reconcile(context.Context, Record, Persist) error { return ErrUnavailable }
func InternalMain([]string) (bool, int)                { return false, 0 }
