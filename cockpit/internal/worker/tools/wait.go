package tools

import (
	"context"
	"fmt"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// Bounds for workerComputer.wait (memql-cockpit#166). The ceiling
// keeps a single dispatch from parking the worker's call slot for
// long stretches -- an agent that needs a longer pause issues
// multiple waits, which keeps each one cancellable and visible in
// the audit stream.
const (
	minWaitMs = 1
	maxWaitMs = 5000
)

// runComputerWait serves workerComputer.wait on EVERY build variant
// -- like `capabilities` it needs no computer-use backend, so the dispatcher
// routes it before the per-build router AND before the platform
// preflights (a pure sleep needs neither Accessibility nor a display
// server). Untagged file: both builds share this one implementation.
//
// Args: { ms } -- clamped into [minWaitMs, maxWaitMs]; an absent /
// non-numeric ms clamps up from 0 to the 1ms floor.
//
// The sleep honors ctx so a cancelled dispatch (worker shutdown,
// server-side call cancellation) returns promptly instead of holding
// the call slot for the full duration.
func runComputerWait(ctx context.Context, args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	ms := waitMsFromArgs(args)
	timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, failure("canceled", fmt.Sprintf("wait canceled after context done: %v", ctx.Err()))
	case <-timer.C:
	}
	return successComputerJSON(
		map[string]any{"ms": ms},
		fmt.Sprintf("waited %dms", ms),
		0,
	), nil
}

// waitMsFromArgs resolves the effective sleep duration from the args
// map. Split from runComputerWait so the clamp table is testable
// without actually sleeping out the 5s ceiling.
func waitMsFromArgs(args map[string]any) int {
	return clampInt(argInt(args, "ms", 0), minWaitMs, maxWaitMs)
}
