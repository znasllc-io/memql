//go:build !linux && !darwin

package appsession

import (
	"fmt"
	"runtime"
)

// platformOpenCommand refuses, by name, on a platform the cockpit has no
// open path for.
//
// This is the behaviour the issue asks for in as many words: report the
// failure rather than silently doing nothing. Windows in particular has a
// real equivalent, and when the cockpit supports Windows at all this is
// where it goes -- until then, a caller learns immediately that this
// machine cannot hand an app to a human, instead of waiting on a window
// nobody will ever see.
func platformOpenCommand(_ string) ([]string, string, error) {
	return nil, "", fmt.Errorf("the cockpit has no `open` path on %s; "+
		"this machine can run a headless session but cannot hand an app to a human", runtime.GOOS)
}
