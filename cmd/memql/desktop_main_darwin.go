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
	if err := tools.RunDesktopMainLoop(func() { worker.HandleCommand(args) }); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
