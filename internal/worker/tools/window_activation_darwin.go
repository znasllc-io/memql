//go:build computeruse && darwin

package tools

/*
#include <stdint.h>
#include <sys/types.h>
extern void *targetActivationObserve(pid_t, uint32_t, int, int, int, int);
extern void targetActivationFree(void *);
extern void targetActivationCancel(void *);
extern int targetActivationPerform(void *);
extern int targetActivationUndisturbed(void *);
extern int targetActivationMatches(void *, uintptr_t, pid_t);
extern void targetMainLoopTurn(void);
*/
import "C"

import (
	"context"
	"fmt"
	"time"
	"unsafe"
)

type darwinWindowActivation struct{ ptr unsafe.Pointer }

func (darwinInputBackend) observeWindow(ctx context.Context, w WindowInfo) (windowActivation, error) {
	a := &darwinWindowActivation{}
	err := desktopMainJobs.call(ctx, func() error {
		a.ptr = C.targetActivationObserve(C.pid_t(w.PID), C.uint32_t(w.ID), C.int(w.X), C.int(w.Y), C.int(w.Width), C.int(w.Height))
		if a.ptr == nil {
			return fmt.Errorf("Cannot retain one exact AX/CG window and stable application identities; no activation occurred.")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}
func (a *darwinWindowActivation) close() { C.targetActivationFree(a.ptr); a.ptr = nil }
func (a *darwinWindowActivation) activate(ctx context.Context) (inputFocus, error) {
	var verified inputFocus
	err := desktopMainJobs.call(ctx, func() error {
		// Cancellation during an AX round trip must be visible before its next
		// activation stage. Join the callback before the retained target is freed.
		cancelled := make(chan struct{})
		stop := context.AfterFunc(ctx, func() { C.targetActivationCancel(a.ptr); close(cancelled) })
		defer func() {
			if !stop() {
				<-cancelled
			}
		}()
		C.targetMainLoopTurn()
		if err := ctx.Err(); err != nil {
			return err
		}
		if code := C.targetActivationPerform(a.ptr); code != 1 {
			return fmt.Errorf("Exact-window activation refused or failed (stage %d); no input was posted.", int(code))
		}
		deadline := time.Now().Add(time.Second)
		for {
			// Permit asynchronous AX/AppKit state to settle after the one request.
			// Never repeat activation. A cancelled accepted job completes here.
			C.targetMainLoopTurn()
			if err := ctx.Err(); err != nil {
				return err
			}
			if C.targetActivationUndisturbed(a.ptr) == 0 {
				return fmt.Errorf("User activity or another activation interrupted window selection.")
			}
			focus, err := (darwinInputBackend{}).focus()
			if err == nil {
				f := focus.(*darwinInputFocus)
				if C.targetActivationMatches(a.ptr, C.uintptr_t(f.ptr.window), C.pid_t(f.window().PID)) != 0 {
					verified = focus
					return nil
				}
				focus.close()
			}
			if !time.Now().Before(deadline) {
				return fmt.Errorf("Requested AX window did not become the verified keyboard target.")
			}
		}
	})
	return verified, err
}
