package worker

import (
	"errors"
	"os"
	"sync/atomic"
)

// The OS may hold a request open while a person answers a dialog. Keep the
// control socket responsive and serialize prompts across both permissions.
type permissionRequestGate struct{ pending atomic.Bool }

var localPermissionRequests permissionRequestGate

func (g *permissionRequestGate) request(permission string, pid int, supported bool, prompt func(string)) error {
	if permission != "accessibility" && permission != "screen_recording" {
		return errors.New("permission must be accessibility or screen_recording")
	}
	if pid != os.Getpid() {
		return errors.New("worker process changed; refresh before requesting permission")
	}
	if !supported {
		return errors.New("permission requests require the macOS computer-use worker")
	}
	if !g.pending.CompareAndSwap(false, true) {
		return errors.New("a macOS permission request is already pending; answer its dialog first")
	}
	go func() {
		defer g.pending.Store(false)
		prompt(permission)
	}()
	return nil // Accepted for processing; never evidence of a grant.
}
