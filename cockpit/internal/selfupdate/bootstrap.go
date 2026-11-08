package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type bootstrapOptions struct {
	release, installed, confirmation string
}

// NewBootstrap is only for the checksum-verified release helper downloaded by
// the one-time upgrade script. It targets an explicitly named installation;
// normal menu updates never accept another executable or a legacy restart.
func NewBootstrap(helperVersion, variant, helper, installed, expected, confirmation string) (*Client, error) {
	if !filepath.IsAbs(installed) || expected == "" || !stableVersion.MatchString(baseVersion(helperVersion)) {
		return nil, errors.New("Bootstrap requires an absolute installed executable and its exact version")
	}
	if confirmation != "" && confirmation != "restart-worker" {
		return nil, errors.New("The only bootstrap confirmation is --confirm=restart-worker")
	}
	if sameExecutable(helper, installed) {
		return nil, errors.New("Run the one-time upgrade from the downloaded release helper, not the installed worker")
	}
	if _, err := compareVersions(baseVersion(helperVersion), expected); err != nil {
		return nil, errors.New("The installed or helper version is not a stable Cockpit version")
	}
	c, err := New(expected, variant, installed)
	if err != nil {
		return nil, err
	}
	c.bootstrap = &bootstrapOptions{release: baseVersion(helperVersion), installed: expected, confirmation: confirmation}
	c.stop = c.stopBootstrapService
	return c, nil
}

func (c *Client) bootstrapCheck(ctx context.Context, requested string) Result {
	r := c.result()
	if requested != c.bootstrap.release {
		return failed(r, errors.New("The requested release does not match this verified upgrade helper"))
	}
	if err := c.verifyBootstrapInstalled(ctx); err != nil {
		return failed(r, err)
	}
	if err := c.recoverBootstrapStop(ctx, requested); err != nil {
		return failed(r, err)
	}
	if recovery, needed := c.recoveryCheck(ctx); needed {
		return recovery
	}
	cmp, err := compareVersions(requested, c.Version)
	if err != nil || cmp < 0 {
		return failed(r, errors.New("Bootstrap requires a newer release; use the installed menu for updates or restart recovery"))
	}
	if cmp == 0 {
		worker, err := c.status(ctx)
		if err != nil || validateWorker(worker, c.Executable) != nil || worker.Version != c.Version {
			return failed(r, errors.New("This version is installed but the matching worker is not ready. Check its saved update recovery or start its existing service"))
		}
		r.OK = true
		r.Phase = "current"
		r.Message = "Cockpit " + requested + " is already installed and running"
		return r
	}
	if err = c.installable(ctx); err != nil {
		return failed(r, err)
	}
	r.OK, r.Available, r.CanInstall = true, true, true
	r.LatestVersion, r.Phase = requested, "available"
	r.ReleaseURL = releaseRoot + "cockpit/v" + requested
	return r
}

// A stop can be interrupted before the atomic exchange. The journal names
// both sides; only that record plus unchanged disk version, service definition
// and home choices authorizes restarting the old job and retrying the upgrade.
func (c *Client) recoverBootstrapStop(ctx context.Context, requested string) error {
	pending, ok := c.pendingRecord()
	if !ok || pending.Version != requested || pending.PreviousVersion != baseVersion(c.Version) || pending.Version == baseVersion(c.Version) {
		return nil
	}
	if c.bootstrap.confirmation != "restart-worker" {
		return errors.New("The previous upgrade stopped before replacement. Finish active work, then retry with --confirm=restart-worker")
	}
	if c.OS == "darwin" {
		if err := c.validateRecoveryPlist(ctx, pending.ServicePlist); err != nil {
			return err
		}
		c.servicePlist = pending.ServicePlist
	}
	if err := c.installable(ctx); err != nil {
		return err
	}
	worker, statusErr := c.status(ctx)
	if statusErr == nil {
		if validateWorker(worker, c.Executable) != nil || worker.Version != c.Version || !sameHomes(worker.Homes, pending.Homes) {
			return errors.New("The interrupted upgrade's worker or saved connections changed; no restart was attempted")
		}
	} else {
		if pending.LegacyPID < 1 || processAlive(pending.LegacyPID) {
			return errors.New("The interrupted legacy worker has not been confirmed stopped. Let it finish shutting down before retrying")
		}
		running, err := c.serviceRunning(ctx)
		if err != nil || running {
			return errors.New("The interrupted worker may still be running; its status must be confirmed before retrying")
		}
		if err = c.startService(ctx); err != nil {
			return err
		}
		if _, err = c.waitReady(ctx, c.Version, pending.Homes); err != nil {
			return err
		}
	}
	c.clearPending()
	return nil
}

func (c *Client) verifyBootstrapInstalled(ctx context.Context) error {
	body, err := c.run(ctx, c.Executable, "--version")
	fields := strings.Fields(strings.TrimSpace(string(body)))
	if err != nil || len(fields) != 3 || fields[0] != "memql" || fields[1] != c.bootstrap.installed || fields[2] != "("+c.Variant+")" {
		return errors.New("The installed worker version or variant changed. Run the upgrade command again")
	}
	return nil
}

func (c *Client) workerService() string {
	return fmt.Sprintf("gui/%d/com.znasllc.memql-worker", os.Getuid())
}

// A legacy worker has no admission gate. Only the person's explicit restart
// confirmation can authorize its stop. Stop the existing manager job before
// swapping files; never synthesize a service, edit its config or force-kill work.
func (c *Client) stopBootstrapService(ctx context.Context, pid int) error {
	if c.bootstrap == nil || c.bootstrap.confirmation != "restart-worker" || pid <= 0 {
		return errors.New("Finish this worker's work, then run the one-time upgrade with --confirm=restart-worker")
	}
	if c.OS == "darwin" {
		c.servicePlist = filepath.Join(c.home, "Library", "LaunchAgents", "com.znasllc.memql-worker.plist")
		if err := c.validateRecoveryPlist(ctx, c.servicePlist); err != nil {
			return err
		}
		body, err := c.run(ctx, "/bin/launchctl", "print", c.workerService())
		if err != nil || !launchdProgram(string(body), c.Executable) || !launchdPID(string(body), pid) {
			return errors.New("The legacy worker service changed before it could stop")
		}
		if _, err = c.run(ctx, "/bin/launchctl", "bootout", c.workerService()); err != nil {
			return errors.New("The legacy worker service could not stop. Nothing was replaced; retry after its status is available")
		}
	} else if c.OS == "linux" {
		body, err := c.run(ctx, "systemctl", "--user", "show", "--property=MainPID", "--value", "memql-worker.service")
		actual, parseErr := strconv.Atoi(strings.TrimSpace(string(body)))
		if err != nil || parseErr != nil || actual != pid {
			return errors.New("The legacy worker service changed before it could stop")
		}
		if _, err := c.run(ctx, "systemctl", "--user", "stop", "memql-worker.service"); err != nil {
			return errors.New("The legacy worker service could not stop. Nothing was replaced")
		}
	} else {
		return errors.New("Bootstrap is supported only on macOS and Linux")
	}
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if !processAlive(pid) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("The legacy worker has not stopped. Nothing was replaced; finish its work before retrying")
		case <-tick.C:
		}
	}
}

func launchdPID(body string, pid int) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == fmt.Sprintf("pid = %d", pid) {
			return true
		}
	}
	return false
}

// Only a saved bootstrap journal may revive an unloaded LaunchAgent. The
// original user-owned plist must still name this exact worker/run command.
func (c *Client) validateRecoveryPlist(ctx context.Context, path string) error {
	if path != filepath.Join(c.home, "Library", "LaunchAgents", "com.znasllc.memql-worker.plist") {
		return errors.New("Unrecognized worker LaunchAgent; the existing installation was kept")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || !ownedByUser(path) {
		return errors.New("The worker LaunchAgent must be a regular file owned by this user")
	}
	for _, field := range []struct{ key, want string }{{"Label", "com.znasllc.memql-worker"}, {"ProgramArguments:0", c.Executable}, {"ProgramArguments:1", "worker"}, {"ProgramArguments:2", "run"}} {
		body, err := c.run(ctx, "/usr/libexec/PlistBuddy", "-c", "Print :"+field.key, path)
		value := strings.TrimSpace(string(body))
		match := value == field.want
		if field.key == "ProgramArguments:0" {
			match = sameExecutable(value, field.want)
		}
		if err != nil || !match {
			return errors.New("The worker LaunchAgent changed or names another installation")
		}
	}
	return nil
}

func (c *Client) recoveryPlist(ctx context.Context) (string, bool) {
	path := c.servicePlist
	if path == "" {
		pending, ok := c.pending()
		if !ok {
			return "", false
		}
		path = pending.ServicePlist
	}
	return path, path != "" && c.validateRecoveryPlist(ctx, path) == nil
}
