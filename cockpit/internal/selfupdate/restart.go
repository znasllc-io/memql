package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// A failed handoff is not permission to restore files or force-restart a job:
// the manager may already have started the verified release and admitted work.
// Callers retain the installed target, recovery journal and previous copy.
type restartRecoveryError struct{ reason string }

func (e *restartRecoveryError) Error() string {
	return "The verified update is installed, but its worker handoff needs recovery. " + e.reason
}

func restartRecovery(reason string) error { return &restartRecoveryError{reason: reason} }

func (c *Client) matchesUpdatedWorker(worker workerStatus, version string, homes []homeStatus) bool {
	return validateWorker(worker, c.Executable) == nil && baseVersion(worker.Version) == baseVersion(version) && sameHomes(worker.Homes, homes)
}

// restartReserved finishes only the exact worker that reserved admission. The
// control request binds the PID inside that process; a replacement rejects it.
// Once it exits, starting the manager job is idempotent. Never issue restart or
// kickstart -k, because either could kill a new worker between observation and
// command execution. A ready replacement may already be busy and is left alone.
func (c *Client) restartReserved(ctx context.Context, before workerStatus, version string) (workerStatus, error) {
	var zero workerStatus
	if validateWorker(before, c.Executable) != nil || c.finish == nil || c.start == nil || c.alive == nil {
		return zero, restartRecovery("The reserved worker could not be identified. Check its status before retrying.")
	}
	current, statusErr := c.status(ctx)
	if statusErr == nil {
		if c.matchesUpdatedWorker(current, version, before.Homes) {
			return current, nil
		}
		if current.PID != before.PID || validateWorker(current, c.Executable) != nil ||
			baseVersion(current.Version) != baseVersion(before.Version) || !sameHomes(current.Homes, before.Homes) {
			return zero, restartRecovery("The worker changed. No replacement worker was stopped; check its status before retrying.")
		}
		if current.Busy == nil || *current.Busy || !current.UpdatePending {
			return zero, restartRecovery("The worker no longer has an idle update reservation. Finish active work, then retry Restart to Finish Update.")
		}
		if err := c.finish(ctx, before.PID); err != nil {
			// A lost reply may follow an accepted shutdown. Never retry against
			// a replacement or use a manager restart as a fallback.
			fresh, freshErr := c.status(ctx)
			if freshErr == nil && c.matchesUpdatedWorker(fresh, version, before.Homes) {
				return fresh, nil
			}
			if c.alive(before.PID) {
				return zero, restartRecovery("The reserved worker did not confirm shutdown. Check its status and retry after its work finishes.")
			}
		}
	} else if c.alive(before.PID) {
		return zero, restartRecovery("The worker is still running but its status is unavailable. Wait for its status before retrying.")
	}
	if err := c.waitReservedWorkerExit(ctx, before.PID); err != nil {
		return zero, restartRecovery("The reserved worker is still shutting down. Wait for it to finish, then retry Restart to Finish Update.")
	}
	// The manager may have started a replacement while the old process exited.
	// Accept only the selected installation with exactly the saved home choices.
	current, statusErr = c.status(ctx)
	if statusErr == nil {
		if c.matchesUpdatedWorker(current, version, before.Homes) {
			return current, nil
		}
		return zero, restartRecovery("Another worker is running with a different version or saved connections. It was left running; check its status.")
	}
	if err := c.start(ctx); err != nil {
		if fresh, statusErr := c.status(ctx); statusErr == nil && c.matchesUpdatedWorker(fresh, version, before.Homes) {
			return fresh, nil
		}
		return zero, restartRecovery("The existing service could not start. Check its status, then retry Restart to Finish Update.")
	}
	ready, err := c.waitReady(ctx, version, before.Homes)
	if err != nil {
		return zero, restartRecovery("The new worker has not confirmed its saved connections. It was not forced to restart; check its status and retry.")
	}
	return ready, nil
}

func (c *Client) waitReservedWorkerExit(ctx context.Context, pid int) error {
	if pid < 1 || c.alive == nil {
		return errors.New("the reserved worker has no process identity")
	}
	wait := c.readyTimeout
	if wait <= 0 {
		wait = 45 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for c.alive(pid) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("reserved worker %d did not exit: %w", pid, ctx.Err())
		case <-tick.C:
		}
	}
	return nil
}
