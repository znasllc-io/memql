//go:build darwin && computeruse

package main

import (
	"fmt"
	"os"
	"runtime"

	"github.com/znasllc-io/memql-cockpit/internal/worker"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// AppKit requires the process's original thread. Only the long-lived desktop
// worker pumps it; ordinary CLI commands and scoped-command children do not.
func init() { runtime.LockOSThread() }

func runWorkerCommand(args []string) {
	if len(args) == 0 || args[0] != "run" {
		worker.HandleCommand(args)
		return
	}
	for _, arg := range args[1:] {
		if arg == "--help" || arg == "-h" {
			worker.HandleCommand(args)
			return
		}
	}
	runWithDesktopLoop(func() { worker.HandleCommand(args) }, tools.RunDesktopMainLoop, tools.DisableDesktopRuntime, func(err error) {
		fmt.Fprintf(os.Stderr, "Desktop actions disabled: %v. Continuing host and inference services.\n", err)
	})
}

// The loop starts the callback only after successful GUI initialization. A
// failure must not take unrelated fleet services down on a headless Mac.
func runWithDesktopLoop(run func(), loop func(func()) error, disable func(), report func(error)) {
	if err := loop(run); err != nil {
		disable()
		report(err)
		run()
	}
}
