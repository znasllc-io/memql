package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// pendingUpdate is written before the atomic replacement, while admission is
// reserved. It carries no credentials, and is never used to rewrite homes.
// Its sole purpose is to prove which installed executable and saved home
// choices a later explicit restart must recover after an updater disappears.
type pendingUpdate struct {
	Version     string       `json:"version"`
	Variant     string       `json:"variant"`
	Executable  string       `json:"executable"`
	Homes       []homeStatus `json:"homes"`
	OperationID string       `json:"operation_id"`
}

func (c *Client) savePending(version, operation string, homes []homeStatus) error {
	pending := pendingUpdate{Version: version, Variant: c.Variant, Executable: c.Executable, Homes: homes, OperationID: operation}
	body, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(c.state, ".pending-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(body); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), filepath.Join(c.state, "pending.json")); err != nil {
		return err
	}
	directory, err := os.Open(c.state)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (c *Client) pending() (pendingUpdate, bool) {
	var pending pendingUpdate
	path := filepath.Join(c.state, "pending.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 64<<10 {
		return pending, false
	}
	body, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(body, &pending) != nil || pending.OperationID == "" ||
		pending.Executable != c.Executable || pending.Variant != c.Variant ||
		!stableVersion.MatchString(pending.Version) || pending.Version != baseVersion(c.Version) ||
		!sameHomes(pending.Homes, pending.Homes) {
		return pendingUpdate{}, false
	}
	return pending, true
}

func (c *Client) clearPending() {
	_ = os.Remove(filepath.Join(c.state, "pending.json"))
}

// recoveryCheck is read-only, including while offline. A newer executable on
// disk is not proof that the service restarted. The pre-swap journal makes the
// restart actionable without redownloading or treating the same version as a
// newer release. If the worker already resumed, normal release checking wins.
func (c *Client) recoveryCheck(ctx context.Context) (Result, bool) {
	pending, ok := c.pending()
	if !ok {
		return Result{}, false
	}
	worker, statusErr := c.status(ctx)
	if statusErr == nil && validateWorker(worker, c.Executable) == nil &&
		baseVersion(worker.Version) == pending.Version && sameHomes(worker.Homes, pending.Homes) {
		return Result{}, false
	}
	r := c.result()
	r.OK = true
	r.RestartRequired = true
	r.LatestVersion = pending.Version
	r.Phase = "restart_required"
	r.Message = "Restart the worker to finish installing Cockpit " + pending.Version
	if err := c.installable(ctx); err != nil {
		r.Message = err.Error()
		return r, true
	}
	if statusErr != nil {
		running, err := c.serviceRunning(ctx)
		if err != nil || running {
			r.Message = "The updated worker is unavailable. Wait for its status, or restart the MemQL worker service after its work finishes"
			return r, true
		}
	} else {
		if err := validateWorker(worker, c.Executable); err != nil {
			r.Message = err.Error()
			return r, true
		}
		if !sameHomes(worker.Homes, pending.Homes) {
			r.Message = "Saved connections changed after the update. Restart the MemQL worker service after its work finishes, then check again"
			return r, true
		}
		if worker.Busy == nil {
			r.Message = "This worker cannot reserve a safe restart. Finish its work and restart the MemQL worker service"
			return r, true
		}
	}
	r.CanInstall = true
	return r, true
}

func (c *Client) resumeUpdate(ctx context.Context, r Result) Result {
	pending, ok := c.pending()
	if !ok || pending.Version != r.LatestVersion {
		return failed(r, errors.New("The pending update changed. Check for updates again"))
	}
	if err := c.installable(ctx); err != nil {
		return failed(r, err)
	}
	before, statusErr := c.status(ctx)
	alreadyReady := false
	reserved := false
	stopped := statusErr != nil
	if statusErr == nil {
		if err := validateWorker(before, c.Executable); err != nil {
			return failed(r, err)
		}
		if !sameHomes(before.Homes, pending.Homes) {
			return failed(r, errors.New("Saved connections changed after the update. Restart the MemQL worker service after its work finishes, then check again"))
		}
		alreadyReady = baseVersion(before.Version) == pending.Version
		if !alreadyReady {
			if before.Busy == nil || *before.Busy {
				return failed(r, errors.New("The worker is busy or cannot reserve a safe restart. Finish its work, then retry"))
			}
			if err := c.prepare(ctx, before.PID); err != nil {
				return failed(r, err)
			}
			reserved = true
		}
	} else {
		running, err := c.serviceRunning(ctx)
		if err != nil || running {
			return failed(r, errors.New("The worker is running but its status cannot be verified. Wait for its status before retrying"))
		}
	}
	defer func() {
		if reserved {
			cancelCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			_ = c.cancel(cancelCtx, before.PID)
		}
	}()
	if !alreadyReady {
		r.OK = true
		r.Phase = "restarting"
		r.Message = "Restarting the worker to finish the interrupted update"
		_ = c.save(r)
		var err error
		if stopped {
			// The manager may auto-start the service after our stopped check.
			// An idempotent start must not kill that newly running worker.
			err = c.startService(ctx)
		} else {
			err = c.restart(ctx)
		}
		if err != nil {
			return failed(r, fmt.Errorf("The update is installed, but its worker could not restart. Retry Restart to Finish Update: %w", err))
		}
		reserved = false
	}
	ready, err := c.waitReady(ctx, pending.Version, pending.Homes)
	if err != nil {
		return failed(r, errors.New("The updated worker has not resumed its saved connections. Check the worker logs, then retry Restart to Finish Update"))
	}
	r.Updated = true
	r.RestartRequired = false
	r.CurrentVersion = pending.Version
	r.Available = false
	r.CanInstall = false
	r.Phase = "reconnecting"
	r.Message = "Reconnecting to your enabled servers"
	_ = c.save(r)
	ready, connectionErr := c.waitConnected(ctx, ready)
	r.Connected, r.Total = connectionCounts(ready.Homes)
	r.WorkerReconnected = r.Connected == r.Total
	r.OK = true
	r.Phase = "complete"
	r.Error = ""
	r.Message = "Cockpit " + pending.Version + " is installed. The worker has resumed its saved connections"
	if !r.WorkerReconnected {
		r.Message = "Cockpit " + pending.Version + " is installed. The worker is retrying its disconnected servers"
	}
	if connectionErr != nil {
		r.Message = "Cockpit " + pending.Version + " is installed. Worker connections could not be verified; check the worker status"
	}
	c.clearPending()
	return r
}
