package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Result is shared by both native menus and the CLI. A completed file update
// and a connected cluster are deliberately distinct facts.
type Result struct {
	OK                bool      `json:"ok"`
	CurrentVersion    string    `json:"current_version"`
	LatestVersion     string    `json:"latest_version,omitempty"`
	Variant           string    `json:"variant"`
	Available         bool      `json:"available"`
	RestartRequired   bool      `json:"restart_required"`
	CanInstall        bool      `json:"can_install"`
	ReleaseURL        string    `json:"release_url,omitempty"`
	Updated           bool      `json:"updated"`
	WorkerReconnected bool      `json:"worker_reconnected"`
	Connected         int       `json:"connected"`
	Total             int       `json:"total"`
	Phase             string    `json:"phase"`
	Message           string    `json:"message,omitempty"`
	Error             string    `json:"error,omitempty"`
	OperationID       string    `json:"operation_id,omitempty"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type homeStatus struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
	State   string `json:"state"`
}
type workerStatus struct {
	Version       string       `json:"version"`
	PID           int          `json:"pid"`
	Executable    string       `json:"executable"`
	Busy          *bool        `json:"busy"`
	Homes         []homeStatus `json:"homes"`
	UpdatePending bool         `json:"update_pending"`
}

// Client owns one installed executable; there is no PATH lookup and no source
// URL supplied by the menu. Test seams are private to this package.
type Client struct {
	Version, Variant, Executable, OS, Arch  string
	home, state, bundle, catalog, downloads string
	http                                    *http.Client
	run                                     func(context.Context, string, ...string) ([]byte, error)
	status                                  func(context.Context) (workerStatus, error)
	prepare                                 func(context.Context, int) error
	cancel                                  func(context.Context, int) error
	restart                                 func(context.Context) error
	readyTimeout, reconnectTimeout          time.Duration
}

func New(version, variant, executable string) (*Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, err
	}
	c := &Client{Version: version, Variant: variant, Executable: executable, OS: runtime.GOOS, Arch: runtime.GOARCH,
		home: home, state: filepath.Join(home, ".memql", "updates"), catalog: catalogURL, downloads: downloadRoot, http: newHTTPClient(),
		readyTimeout: 45 * time.Second, reconnectTimeout: 45 * time.Second}
	if c.OS == "darwin" && filepath.Base(executable) == "MemQL" && filepath.Base(filepath.Dir(executable)) == "MacOS" {
		app := filepath.Dir(filepath.Dir(filepath.Dir(executable)))
		if filepath.Ext(app) == ".app" {
			c.bundle = app
		}
	}
	c.run = runCommand
	c.status = func(ctx context.Context) (workerStatus, error) { return c.control(ctx, "status", 0) }
	c.prepare = func(ctx context.Context, pid int) error {
		s, e := c.control(ctx, "prepare-update", pid)
		if e == nil && !s.UpdatePending {
			return errors.New("The worker did not reserve the update. Update the worker before retrying")
		}
		return e
	}
	c.cancel = func(ctx context.Context, pid int) error { _, e := c.control(ctx, "cancel-update", pid); return e }
	c.restart = c.restartService
	return c, nil
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// Never echo a service's environment or subprocess output on failure.
	var out cappedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.data, err
}

type cappedBuffer struct{ data []byte }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > 2<<20 {
		return 0, errors.New("Command output exceeded limit")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func (c *Client) control(ctx context.Context, action string, pid int) (workerStatus, error) {
	args := []string{"worker", "control", "--action=" + action}
	if pid > 0 {
		args = append(args, fmt.Sprintf("--pid=%d", pid))
	}
	body, err := c.run(ctx, c.Executable, args...)
	var reply struct {
		OK     bool          `json:"ok"`
		Error  string        `json:"error"`
		Status *workerStatus `json:"status"`
	}
	if json.Unmarshal(body, &reply) != nil {
		return workerStatus{}, errors.New("The background worker is unavailable. Start it and retry the update")
	}
	if !reply.OK || reply.Status == nil || err != nil {
		if reply.Error != "" {
			return workerStatus{}, errors.New(reply.Error)
		}
		return workerStatus{}, errors.New("The worker did not accept the update request")
	}
	return *reply.Status, nil
}

func (c *Client) result() Result {
	return Result{CurrentVersion: c.Version, Variant: c.Variant, Phase: "idle", UpdatedAt: time.Now().UTC()}
}
func failed(r Result, err error) Result {
	r.OK = false
	r.Phase = "error"
	r.Error = err.Error()
	r.Message = ""
	r.UpdatedAt = time.Now().UTC()
	return r
}

func (c *Client) Check(ctx context.Context) Result {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if recovery, needed := c.recoveryCheck(ctx); needed {
		return recovery
	}
	r := c.result()
	rel, err := c.latest(ctx)
	if err != nil {
		return failed(r, err)
	}
	r.LatestVersion = strings.TrimPrefix(rel.Tag, "cockpit/v")
	r.ReleaseURL = releaseRoot + rel.Tag
	cmp, err := compareVersions(r.LatestVersion, c.Version)
	if err != nil {
		return failed(r, err)
	}
	r.OK = true
	r.Available = cmp > 0
	r.Phase = "current"
	if !r.Available {
		r.Message = "Cockpit is up to date"
		return r
	}
	r.Phase = "available"
	asset, sums := c.assetNames(r.LatestVersion)
	if !hasAsset(rel, asset) || !hasAsset(rel, sums) {
		r.Message = "This release is still being prepared for your computer. Check again later"
		return r
	}
	if err = c.installable(ctx); err != nil {
		r.Message = err.Error()
		return r
	}
	r.CanInstall = true
	r.Message = "Cockpit " + r.LatestVersion + " is available"
	return r
}

func (c *Client) Status() Result {
	r := c.result()
	body, err := os.ReadFile(filepath.Join(c.state, "status.json"))
	if os.IsNotExist(err) {
		r.OK = true
		return r
	}
	if err != nil || len(body) > 64<<10 || json.Unmarshal(body, &r) != nil {
		return failed(c.result(), errors.New("The last update status could not be read"))
	}
	// A killed updater cannot keep a permanent progress spinner. The advisory
	// lock is the authority; a receipt only describes what its owner last did.
	if r.Phase != "complete" && r.Phase != "error" && !updateLocked(c.state) {
		return failed(r, errors.New("The update was interrupted. Check the worker and retry"))
	}
	return r
}

func (c *Client) save(r Result) error {
	r.UpdatedAt = time.Now().UTC()
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(c.state, ".status-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(c.state, "status.json"))
}

func (c *Client) Install(ctx context.Context, requested string) (r Result) {
	r = c.result()
	r.OperationID = operationID()
	if !stableVersion.MatchString(requested) {
		return failed(r, errors.New("Choose a stable Cockpit release version"))
	}
	unlock, err := lockUpdate(c.state)
	if err != nil {
		return failed(r, err)
	}
	defer unlock()
	defer func() {
		r.UpdatedAt = time.Now().UTC()
		if err := c.save(r); err != nil && r.OK {
			r = failed(r, errors.New("The update completed, but its receipt could not be saved"))
		}
	}()
	progress := func(phase, message string) { r.OK = true; r.Phase = phase; r.Message = message; _ = c.save(r) }
	ctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
	defer cancel()
	progress("starting", "Checking the selected release")
	check := c.Check(ctx)
	r.LatestVersion = check.LatestVersion
	r.ReleaseURL = check.ReleaseURL
	r.Available = check.Available
	r.CanInstall = check.CanInstall
	r.RestartRequired = check.RestartRequired
	if !check.OK {
		return failed(r, errors.New(check.Error))
	}
	if !check.Available && !check.RestartRequired {
		return failed(r, errors.New("Cockpit is already current; no update was installed"))
	}
	if check.LatestVersion != requested {
		return failed(r, errors.New("The available release changed. Check for updates again before installing"))
	}
	if !check.CanInstall {
		return failed(r, errors.New(check.Message))
	}
	if check.RestartRequired {
		return c.resumeUpdate(ctx, r)
	}
	before, err := c.status(ctx)
	if err != nil {
		return failed(r, err)
	}
	if err = validateWorker(before, c.Executable); err != nil {
		return failed(r, err)
	}
	if before.Busy == nil {
		return failed(r, errors.New("This worker cannot reserve a safe update. Install the current Cockpit release once using the installer"))
	}
	if *before.Busy {
		return failed(r, errors.New("The worker is busy. Wait for its current work to finish, then retry"))
	}
	target := c.Executable
	if c.bundle != "" {
		target = c.bundle
	}
	stage, err := os.MkdirTemp(filepath.Dir(target), ".memql-update-*")
	if err != nil {
		return failed(r, errors.New("Cannot stage this update beside the installed app. Reinstall using an account with write access"))
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(stage)
		}
	}()
	asset, sums := c.assetNames(requested)
	base := c.downloads + "cockpit/v" + requested + "/"
	progress("downloading", "Downloading Cockpit "+requested)
	manifest, _, err := c.get(ctx, base+sums, 2<<20)
	if err != nil {
		return failed(r, err)
	}
	digest, err := expectedChecksum(manifest, asset)
	if err != nil {
		return failed(r, err)
	}
	archive := filepath.Join(stage, asset)
	if err = c.download(ctx, base+asset, archive, digest); err != nil {
		return failed(r, err)
	}
	progress("verifying", "Verifying the downloaded release")
	incoming := archive
	if strings.HasSuffix(asset, ".tar.gz") {
		unpacked := filepath.Join(stage, "unpacked")
		if err = extract(archive, unpacked); err != nil {
			return failed(r, err)
		}
		incoming = filepath.Join(unpacked, "memql")
		if c.bundle != "" {
			incoming = filepath.Join(unpacked, "MemQL.app")
		}
	}
	worker := incoming
	if c.bundle != "" {
		worker = filepath.Join(incoming, "Contents", "MacOS", "MemQL")
	}
	if c.bundle != "" {
		if err = c.verifyBundle(ctx, incoming); err != nil {
			return failed(r, err)
		}
	}
	if err = os.Chmod(worker, 0755); err != nil {
		return failed(r, errors.New("The release is missing its worker executable"))
	}
	body, err := c.run(ctx, worker, "--version")
	if err != nil || strings.TrimSpace(string(body)) != "memql "+requested+" ("+c.Variant+")" && !validBinaryVersion(string(body), requested, c.Variant) {
		return failed(r, errors.New("The downloaded worker version or build variant does not match this installation"))
	}
	// Download and validation may take minutes. Reserve immediately before the
	// swap: the worker atomically refuses active work and closes admission. Its
	// bounded lease expires if this updater dies; no saved policy is modified.
	latest, err := c.status(ctx)
	if err != nil {
		return failed(r, err)
	}
	if latest.PID != before.PID || !sameHomes(before.Homes, latest.Homes) {
		return failed(r, errors.New("The worker changed while the update downloaded. Retry"))
	}
	if err = c.installable(ctx); err != nil {
		return failed(r, err)
	}
	if err = c.prepare(ctx, before.PID); err != nil {
		return failed(r, err)
	}
	reserved := true
	defer func() {
		if reserved {
			cancelCtx, stop := context.WithTimeout(context.Background(), 8*time.Second)
			defer stop()
			_ = c.cancel(cancelCtx, before.PID)
		}
	}()
	if err = c.savePending(requested, r.OperationID, before.Homes); err != nil {
		return failed(r, errors.New("Could not save update recovery information. Nothing was replaced"))
	}
	progress("installing", "Installing the verified release")
	previous := filepath.Join(stage, "previous")
	if err = swap(target, incoming, previous); err != nil {
		// A failed exchange may have retained the old app under incoming rather
		// than previous. Preserve the entire private stage on every swap error.
		cleanup = false
		return failed(r, err)
	}
	rollback := func(cause error) Result {
		// Keep both copies if restoration fails. Losing a rollback copy is worse
		// than retaining an identifiable directory for manual repair.
		if e := restore(target, previous); e != nil {
			cleanup = false
			return failed(r, fmt.Errorf("%s. Could not restore the previous installation; recovery files: %s", cause, stage))
		}
		if e := c.savePending(baseVersion(before.Version), r.OperationID, before.Homes); e != nil {
			return failed(r, fmt.Errorf("%s. Previous files restored, but restart recovery could not be recorded. Restart the MemQL worker service", cause))
		}
		rollbackCtx, stop := context.WithTimeout(context.Background(), 60*time.Second)
		defer stop()
		if e := c.restart(rollbackCtx); e != nil {
			return failed(r, fmt.Errorf("%s. Previous files restored, but the worker needs to be restarted", cause))
		}
		reserved = false
		if _, e := c.waitReady(rollbackCtx, before.Version, before.Homes); e != nil {
			return failed(r, fmt.Errorf("%s. Previous files restored, but worker recovery could not be confirmed", cause))
		}
		c.clearPending()
		return failed(r, fmt.Errorf("%s. The previous version was restored and restarted", cause))
	}
	progress("restarting", "Restarting the worker with its saved connections")
	if err = c.restart(ctx); err != nil {
		return rollback(err)
	}
	reserved = false
	ready, err := c.waitReady(ctx, requested, before.Homes)
	if err != nil {
		return rollback(err)
	}
	r.Updated = true
	r.CurrentVersion = requested
	r.Available = false
	r.CanInstall = false
	progress("reconnecting", "Reconnecting to your enabled servers")
	ready, connectionErr := c.waitConnected(ctx, ready)
	r.Connected, r.Total = connectionCounts(ready.Homes)
	r.WorkerReconnected = r.Connected == r.Total
	r.OK = true
	r.Phase = "complete"
	r.Error = ""
	if r.WorkerReconnected {
		r.Message = "Cockpit " + requested + " is installed. The worker has resumed its saved connections"
	} else {
		r.Message = "Cockpit " + requested + " is installed. The worker is retrying its disconnected servers"
	}
	if connectionErr != nil {
		r.Message = "Cockpit " + requested + " is installed. Worker connections could not be verified; check the worker status"
	}
	if c.OS == "darwin" {
		r.Message += ". macOS may ask you to approve permissions for this release"
	}
	c.clearPending()
	return r
}

func validBinaryVersion(output, version, variant string) bool {
	fields := strings.Fields(strings.TrimSpace(output))
	return len(fields) == 3 && fields[0] == "memql" && baseVersion(fields[1]) == version && fields[2] == "("+variant+")"
}

func validateWorker(s workerStatus, path string) error {
	actual, err := filepath.EvalSymlinks(s.Executable)
	if err != nil || actual != path || s.PID < 1 {
		return errors.New("This menu and the running worker use different installations. Open the menu belonging to the running worker")
	}
	return nil
}
func sameHomes(a, b []homeStatus) bool {
	if len(a) != len(b) {
		return false
	}
	expected := map[string]bool{}
	for _, h := range a {
		if _, ok := expected[h.ID]; ok {
			return false
		}
		expected[h.ID] = h.Enabled
	}
	for _, h := range b {
		enabled, ok := expected[h.ID]
		if !ok || enabled != h.Enabled {
			return false
		}
		delete(expected, h.ID)
	}
	return len(expected) == 0
}
func connectionCounts(homes []homeStatus) (connected, total int) {
	for _, h := range homes {
		if h.Enabled {
			total++
			if h.State == "Connected" {
				connected++
			}
		}
	}
	return
}

func (c *Client) waitReady(ctx context.Context, version string, homes []homeStatus) (workerStatus, error) {
	deadline := time.NewTimer(c.readyTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		s, err := c.status(ctx)
		if err == nil && validateWorker(s, c.Executable) == nil && baseVersion(s.Version) == baseVersion(version) && sameHomes(homes, s.Homes) {
			return s, nil
		}
		select {
		case <-ctx.Done():
			return workerStatus{}, ctx.Err()
		case <-deadline.C:
			return workerStatus{}, errors.New("The new worker did not resume with its saved server settings")
		case <-ticker.C:
		}
	}
}
func (c *Client) waitConnected(ctx context.Context, s workerStatus) (workerStatus, error) {
	expected := s
	deadline := time.NewTimer(c.reconnectTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		connected, total := connectionCounts(s.Homes)
		if connected == total {
			return s, nil
		}
		select {
		case <-ctx.Done():
			return s, nil
		case <-deadline.C:
			return s, nil
		case <-ticker.C:
			fresh, err := c.status(ctx)
			if err != nil || validateWorker(fresh, c.Executable) != nil || fresh.PID != expected.PID ||
				baseVersion(fresh.Version) != baseVersion(expected.Version) || !sameHomes(fresh.Homes, expected.Homes) {
				// Never turn a different process, dropped home or stale prior
				// observation into a successful reconnection receipt.
				s.Homes = append([]homeStatus(nil), s.Homes...)
				for i := range s.Homes {
					if s.Homes[i].Enabled {
						s.Homes[i].State = "Unknown"
					}
				}
				return s, errors.New("The restarted worker's connections could not be verified")
			}
			s = fresh
		}
	}
}

func swapFile(target, incoming, previous string) error {
	// A hard link retains the old inode while rename replaces the executable
	// atomically. At no point can a service restart find its executable absent.
	if err := os.Link(target, previous); err != nil {
		return errors.New("Could not retain the previous executable; nothing was replaced")
	}
	if err := os.Rename(incoming, target); err != nil {
		return errors.New("Could not replace Cockpit; the previous executable is still installed")
	}
	return nil
}

func (c *Client) download(ctx context.Context, address, path, digest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "MemQL-Cockpit-Updater/"+c.Version)
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("The Cockpit download failed. Check your connection and retry")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Cockpit download returned HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(resp.Body, (1<<30)+1))
	if err != nil {
		return errors.New("The Cockpit download was interrupted")
	}
	if n > 1<<30 {
		return errors.New("The Cockpit download exceeded its size limit")
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("Cockpit checksum verification failed. Nothing was installed")
	}
	return f.Sync()
}

func extract(archive, destination string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	if err = os.MkdirAll(destination, 0700); err != nil {
		return err
	}
	var total int64
	count := 0
	seen := map[string]bool{}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.New("The Cockpit archive is damaged")
		}
		count++
		name := filepath.Clean(header.Name)
		if count > 10000 || header.Size < 0 || header.Size > (1<<30)-total || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) || strings.Contains(name, `\`) || seen[name] {
			return errors.New("The Cockpit archive contains unsafe entries")
		}
		total += header.Size
		seen[name] = true
		path := filepath.Join(destination, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err = os.MkdirAll(path, 0755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return err
			}
			out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(header.Mode)&0755)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(out, tr, header.Size)
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return errors.New("The Cockpit archive contains links or special files")
		}
	}
}
