package backup

import (
	"context"
	"errors"
	"fmt"
	"github.com/znasllc-io/memql-cockpit/internal/worker/transfer"
	"io/fs"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The watched-folder backup: one folder on this machine, kept arriving in the
// Library (memql#4841, the cockpit half of epic memql#4783).
//
// Existing retain-mode arrangements keep deleted originals in Files. Explicit
// sync-mode arrangements move confirmed deletions to the Bin. Both are one-way:
// this sweeper never changes source files on the computer.
//
// ===========================================================================
// WHO DECIDES WHAT
// ===========================================================================
// The GRAPH says which folder, where it files, what to skip -- so a person can
// set a backup up from a browser on a different machine while this one is
// asleep. THIS MACHINE decides whether it will do it at all: every path is
// checked against policy.yaml's backup.roots before anything is read, and a
// refusal is REPORTED rather than silent, because a machine that quietly
// ignored a watch would be indistinguishable from one that was offline.

const (
	// DefaultSweepInterval is how often a watch is walked when nothing is
	// wrong. Five minutes is chosen against what a person notices: a file
	// saved and then looked for in the Library is worth waiting a few minutes
	// for, and a folder of video is not worth walking every thirty seconds.
	DefaultSweepInterval = 5 * time.Minute

	// verifyRefreshInterval is how often an UNCHANGED file's `synced` is
	// re-stamped so `linkCheckedAt` does not silently mean "as of three weeks
	// ago".
	//
	// A DAY, not a sweep. A state CHANGE is reported immediately; this paces
	// only the repeats. Without it, a watched folder of ten thousand files
	// would be ten thousand graph writes every five minutes, forever, to say
	// nothing at all -- and the engine's own field doc warns that touching
	// linkCheckedAt is what strobes the Files list.
	verifyRefreshInterval = 24 * time.Hour

	// initialDelay lets the worker finish registering and settle before the
	// first walk, so a laptop waking up does not start hashing immediately.
	initialDelay = 20 * time.Second
)

// Options wires the manager. Shaped after appsession.Options for the reason
// that package's own header gives: one struct the caller fills from policy and
// config, with the seams the tests replace.
type Options struct {
	CheckDownloadPath func(string) error
	Transfers         *transfer.Runner
	Logger            *slog.Logger
	// StateDir is where the per-watch ledgers live.
	StateDir string
	// BaseURL is the cluster's HTTP origin. Derived from the worker's own
	// cluster_url by the caller -- there is no second host to configure.
	BaseURL string
	// Bearer resolves the signed-in user's access token. Called per request
	// so a token that rolls forward mid-sweep is picked up.
	Bearer func(context.Context) (string, error)
	// CheckPath is this machine's veto. A nil check REFUSES EVERYTHING, which
	// is the safe direction: a wiring mistake must not become "back up
	// anything the cluster names".
	CheckPath func(path string) error
	// SweepInterval overrides DefaultSweepInterval. Tests set it.
	SweepInterval time.Duration
	// HTTPClient is shared by both clients. Tests point it at an httptest
	// server.
	HTTPClient *http.Client
}

// Manager runs the sweep loop.
type Manager struct {
	transfers *transfer.Runner
	rpc       *transfer.RPC
	opts      Options
	graph     *Graph
	library   *Library
	logger    *slog.Logger

	mu      sync.Mutex
	ledgers map[string]*Ledger

	// workerID is the registration this sweep is acting as. Written by
	// SweepOnce from its own parameter and read by the push path beneath it,
	// all on one goroutine per sweep.
	workerID string
}

// New builds a manager, or nil when it has nothing to work with.
//
// A NIL MANAGER IS A WORKING ONE that does nothing, so the caller wires it
// unconditionally and every method is nil-safe. A machine with no signed-in
// user is the ordinary state of a freshly paired worker, and it must not be a
// startup failure.
func New(opts Options) *Manager {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.SweepInterval <= 0 {
		opts.SweepInterval = DefaultSweepInterval
	}
	if strings.TrimSpace(opts.BaseURL) == "" || opts.Bearer == nil {
		return nil
	}
	var rpc *transfer.RPC
	transfers := opts.Transfers
	if transfers == nil {
		rpc = &transfer.RPC{Endpoint: opts.BaseURL, Bearer: opts.Bearer}
		transfers = &transfer.Runner{Control: rpc, StateDir: opts.StateDir, CheckRead: opts.CheckPath, CheckWrite: opts.CheckDownloadPath, HTTP: opts.HTTPClient}
	}
	return &Manager{
		transfers: transfers, rpc: rpc,
		opts:    opts,
		graph:   NewGraph(opts.BaseURL, opts.HTTPClient, opts.Bearer),
		library: NewLibrary(opts.BaseURL, opts.HTTPClient, opts.Bearer),
		logger:  opts.Logger,
		ledgers: map[string]*Ledger{},
	}
}

// Run sweeps until the context is cancelled.
//
// A TICKER, in the shape heartbeatLoop established, and every failure inside
// one pass is logged and dropped rather than returned: a cluster that is
// briefly unreachable, or a folder on an unmounted drive, must not end the
// loop -- the next tick is the retry, and it is already scheduled.
// `registrationID` is a GETTER, not a value, because the id is not known when
// this starts: it arrives on the RegisterAck, inside a reconnect loop that may
// still be backing off. A sweep with no id is SKIPPED and the next tick asks
// again -- which is also the right behaviour for a machine whose registration
// was revoked while it was running.
func (m *Manager) Run(ctx context.Context, registrationID func() string) {
	if m == nil || registrationID == nil {
		return
	}
	if m.rpc != nil {
		defer m.rpc.Close()
	}
	transferCtx, stopTransfers := context.WithCancel(ctx)
	defer stopTransfers()
	transfersDone := make(chan struct{})
	go func() { defer close(transfersDone); m.transfers.RunPending(transferCtx, registrationID) }()
	defer func() { stopTransfers(); <-transfersDone }()
	if !sleepFor(ctx, initialDelay) {
		return
	}
	ticker := time.NewTicker(m.opts.SweepInterval)
	defer ticker.Stop()
	announced := false
	lastSaved := ""
	for {
		if id := strings.TrimSpace(registrationID()); id != "" {
			if id != lastSaved {
				// Recorded so `memql worker backup --once` acts as the same
				// machine. Best-effort: losing it costs that command its id
				// and costs this sweep nothing.
				if err := SaveRegistrationID(m.opts.StateDir, id); err != nil {
					m.logger.Debug("backup: could not record this machine's registration id", "error", err)
				}
				lastSaved = id
			}
			m.sweep(ctx, id, false)
		} else if !announced {
			// Said ONCE. A machine that has not registered yet is the ordinary
			// state for the first few seconds, and logging it every five
			// minutes forever would be the only line in a quiet log.
			m.logger.Info("backup: waiting for this machine to register before sweeping")
			announced = true
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// SweepOnce runs one pass over every watch this machine holds. Exported so
// `memql worker backup --once` runs EXACTLY what the loop runs -- the
// discipline models_cmd.go states: a second implementation could disagree
// with the first and the operator would have no way to know which one lied.
//
// THE REGISTRATION ID IS A PARAMETER, not a field read off the manager, and it
// is REFUSED when blank. Both halves matter. As a field it was set only inside
// Run, so the `--once` command -- which never calls Run -- swept with the empty
// string, matched no rows, pushed nothing and printed "Done"; and a push that
// did happen would have carried `uploadedFromWorkerId: ""`, which the engine
// refuses. A parameter makes the id impossible to forget, and the guard makes
// a forgotten one loud rather than a silent no-op that reports success.
func (m *Manager) SweepOnce(ctx context.Context, workerID string) {
	m.sweep(ctx, workerID, true)
}

// Scheduled polling shares the same pass, but skips folders that are not due.
// An explicit --once request bypasses the schedule, never pause or policy.
func (m *Manager) sweep(ctx context.Context, workerID string, force bool) {
	if m == nil {
		return
	}
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		m.logger.Warn("backup: no registration id, so nothing was swept -- a push cannot name its machine without one")
		return
	}
	unlock, err := transfer.LockLocal(filepath.Join(m.opts.StateDir, "backup", "sweep.lock"))
	if err != nil {
		m.logger.Debug("backup: another sweep is active", "error", err)
		return
	}
	defer unlock()
	// Another process may have completed a sweep since our last pass.
	m.mu.Lock()
	m.ledgers = map[string]*Ledger{}
	m.mu.Unlock()
	m.workerID = workerID
	watches, err := m.graph.Watches(ctx, workerID)
	if err != nil {
		m.logger.Warn("backup: could not read this machine's watched folders", "error", err)
		return
	}
	for _, watch := range watches {
		if err := ctx.Err(); err != nil {
			return
		}
		if !watch.Active() || (!force && !watch.Due(time.Now())) {
			// Paused. Nothing is scanned, nothing is reported, and the ledger
			// is deliberately LEFT ALONE -- resuming must not re-push a folder
			// somebody paused for an afternoon.
			continue
		}
		m.sweepWatch(ctx, watch)
	}
}

func (m *Manager) sweepWatch(ctx context.Context, watch Watch) {
	log := m.logger.With("watch", watch.ID, "path", watch.LocalPath)

	// THE VETO RUNS FIRST, before the path is even stat'd. A folder this
	// machine has not been told to back up is not a folder to go looking at.
	check := m.opts.CheckPath
	if check == nil {
		m.report(ctx, watch.ID, "refused_by_policy", 0, 0,
			"this machine has no backup policy configured, so it backs up nothing")
		return
	}
	if err := check(watch.LocalPath); err != nil {
		m.report(ctx, watch.ID, "refused_by_policy", 0, 0, err.Error())
		return
	}

	// The same veto, applied to every entry beneath the root rather than only
	// to the root itself.
	var scan ScanResult
	var err error
	if watch.Mode == "sync" {
		scan, err = scanFolder(watch.LocalPath, watch.ExcludeGlobs, watch.IncludeHidden, check, false)
	} else {
		scan, err = Scan(watch.LocalPath, watch.ExcludeGlobs, watch.IncludeHidden, check)
	}
	if err != nil {
		state := "unreadable"
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			state = "missing"
		}
		m.report(ctx, watch.ID, state, 0, 0, err.Error())
		// A missing root may be an unplugged drive. Keep its durable copies
		// and report unavailable rather than claiming each original was deleted.
		m.flagAllUnavailable(ctx, watch)
		return
	}

	if watch.Mode == "sync" && scan.Bytes > transfer.MaxBytes {
		m.report(ctx, watch.ID, "ok", len(scan.Entries), scan.Bytes, "A synced folder cannot exceed 1 TB. No changes were sent.")
		return
	}
	ledger := m.ledgerFor(watch.ID)
	// DEFERRED, so a sweep cut short by a cancelled context still keeps the
	// record of what it already sent. Saving only at the end made the unit of
	// loss a whole SWEEP: every file uploaded before the deadline was
	// forgotten, and the next run re-pushed all of them, versioning each one
	// again. A push whose ledger write is lost is safe (the (machine, path)
	// key versions rather than duplicates) -- but safe is not free.
	defer func() {
		if err := ledger.Save(); err != nil {
			log.Warn("backup: could not save this machine's record of what it sent", "error", err)
		}
	}()
	if watch.Mode != "sync" {
		m.followMoves(ctx, ledger, scan)
	}
	var pushErr string
	pushed := 0
	for _, entry := range scan.Entries {
		if err := ctx.Err(); err != nil {
			return
		}
		var changed bool
		var err error
		if watch.Mode == "sync" {
			changed, err = m.pushSync(ctx, watch, ledger, entry)
		} else {
			changed, err = m.pushIfChanged(ctx, watch, ledger, entry)
		}
		if err != nil {
			// The FIRST failure is what is reported; the sweep carries on, so
			// one file the cluster refuses does not stop the other thousand.
			if pushErr == "" {
				pushErr = err.Error()
			}
			log.Warn("backup: a file did not arrive", "file", entry.Rel, "error", err)
			continue
		}
		if changed {
			pushed++
		}
	}

	if watch.Mode == "sync" {
		m.syncDeletions(ctx, watch, ledger, scan)
	} else {
		m.verify(ctx, ledger, scan)
	}

	state := "ok"
	message := pushErr
	if scan.Skipped > 0 && message == "" {
		// A partly-unreadable folder reports ok with a count rather than
		// `unreadable`: most of it worked, and calling the whole watch
		// unreadable would hide the files that did arrive.
		message = "some entries could not be read on this machine"
	}
	m.report(ctx, watch.ID, state, len(scan.Entries), scan.Bytes, message)
	if pushed > 0 {
		log.Info("backup: files arrived", "count", pushed, "seen", len(scan.Entries))
	}
}

// pushIfChanged sends a file only when its bytes actually differ.
//
// THREE GATES, CHEAPEST FIRST, and the order is the whole performance story:
// a stat that matches costs nothing; a stat that moved but a digest that did
// not costs one read and no upload (a touched file, a copy restored, a
// timestamp changed by a backup tool); only a digest that moved sends bytes.
func (m *Manager) pushIfChanged(ctx context.Context, watch Watch, ledger *Ledger, entry Entry) (bool, error) {
	prior, known := ledger.Get(entry.Path)
	if known && prior.Stamp.Unchanged(entry.Stamp) {
		return false, nil
	}
	// RE-STAT FIRST, and push what THIS stat saw.
	//
	// The walk's stamp can be minutes or hours old by the time a big folder
	// gets here, and the declared size is what the session route chunks
	// against. Declaring a stale, SMALLER size sends only that many bytes of a
	// file that has since grown -- and the engine's commit check passes,
	// because staged bytes really do equal the size it was told. The result is
	// a silently truncated copy. Worse, the digest below was of the CURRENT
	// content, so the next sweep hashes the same bytes, matches, and never
	// sends the rest: truncated forever, reported `synced`.
	//
	// One observation, used for the size, the digest and the record. A file
	// that changes DURING the push still converges: the recorded stamp will
	// not match the next sweep's stat, so it is pushed again.
	if m.opts.CheckPath == nil {
		return false, fmt.Errorf("backup: no local path policy")
	}
	if err := m.opts.CheckPath(entry.Path); err != nil {
		return false, err
	}
	sourcePath, fresh, err := snapshot(ctx, m.opts.StateDir, entry.Path)
	if err != nil {
		return false, err
	}
	defer os.Remove(sourcePath)
	digest := fresh.SHA256
	if prior.VersionNumber == 0 {
		remote, readErr := m.graph.FileAt(ctx, m.workerID, entry.Path)
		if readErr != nil {
			return false, readErr
		}
		if remote != nil {
			remoteDigest := rowString(remote, "sha256")
			if remoteDigest == "" || (remoteDigest != digest && remoteDigest != prior.Stamp.SHA256) {
				fileID := rowString(remote, "id")
				if fileID != "" {
					_ = m.graph.SetLinkState(ctx, fileID, "conflict")
				}
				return false, fmt.Errorf("backup: the stored copy has no matching local baseline; keep both copies and relink explicitly")
			}
			version, parseErr := strconv.Atoi(fmt.Sprint(rowField(remote, "versionNumber")))
			if parseErr != nil || version < 1 {
				version = 1
			}
			prior.FileID = rowString(remote, "id")
			prior.VersionNumber = version
			if remoteDigest == digest {
				prior.Stamp = fresh
				prior.LinkState = "synced"
				ledger.Put(entry.Path, prior)
				return false, ledger.Save()
			}
		}
	}
	if known && prior.Stamp.SHA256 == digest && prior.FileID != "" {
		prior.Stamp = fresh
		ledger.Put(entry.Path, prior)
		return false, nil
	}
	resumeID := ""
	if known && prior.UploadSize == fresh.Size && prior.UploadSHA256 == digest {
		resumeID = prior.UploadID
	}
	remember := func(id string) error {
		prior.UploadID = id
		prior.UploadSize = fresh.Size
		prior.UploadSHA256 = digest
		ledger.Put(entry.Path, prior)
		// Persist BEFORE the first chunk: SIGKILL or power loss must not
		// forget a partially completed multi-gigabyte upload.
		return ledger.Save()
	}
	var result PushResult
	if fresh.Size <= m.library.oneShotLimit {
		result, err = m.library.pushSnapshotOneShot(ctx, m.workerID, watch.ID, entry.Path, sourcePath, watch.FolderID, prior.VersionNumber)
	} else {
		result, err = m.library.pushSession(ctx, m.workerID, watch.ID, entry.Path, sourcePath, watch.FolderID, fresh.Size, resumeID, prior.VersionNumber, remember)
	}
	if err != nil {
		var failure *httpFailure
		if errors.As(err, &failure) && failure.status == 409 && !strings.Contains(err.Error(), "origin_trashed") && prior.FileID != "" {
			prior.LinkState = "conflict"
			ledger.Put(entry.Path, prior)
			_ = m.graph.SetLinkState(ctx, prior.FileID, "conflict")
		}
		return false, err
	}
	stamp := fresh
	stamp.SHA256 = digest
	now := time.Now().UTC().Unix()
	// No UploadID: the session is spent, and a completed one would only ever
	// cost the next push a wasted inventory read.
	ledger.Put(entry.Path, Record{
		Stamp:          stamp,
		FileID:         result.FileID,
		VersionNumber:  result.VersionNumber,
		PushedAtUnix:   now,
		VerifiedAtUnix: now,
		// The ENGINE stamps `synced` on any push naming a (machine, path), so
		// recording it here is recording what the server just did -- not a
		// claim this side made. Writing it again over the wire would be a
		// second write saying the same thing.
		LinkState: "synced",
	})
	if err := ledger.Save(); err != nil {
		return true, err
	}
	return true, nil
}

// verify is the origin-liveness lane: the two states only something looking at
// the machine can give, plus the paced re-stamp of the one the engine gave.
//
// IT NEEDS THE ENTRIES, NOT JUST THEIR PATHS, and that is what makes `stale`
// reachable at all. The first version compared presence only, so it could
// answer `synced` or `origin_gone` and nothing else -- which meant a file whose
// push had FAILED (out of storage, a 401, a transient 5xx) kept reporting
// `synced`, because the ledger still held the last successful push's state and
// nothing contradicted it. The Library went on claiming a copy was current
// when the machine knew it was not, which is the exact claim this state exists
// to prevent, and the Files app's "Catching up" tone was unreachable.
//
// Comparing the ORIGIN's current stamp against the last one actually PUSHED is
// the honest test: they differ exactly when there are bytes here that have not
// arrived there.
func (m *Manager) verify(ctx context.Context, ledger *Ledger, scan ScanResult) {
	present := make(map[string]Stamp, len(scan.Entries))
	for _, entry := range scan.Entries {
		present[entry.Path] = entry.Stamp
	}
	now := time.Now().UTC().Unix()

	for _, path := range ledger.Paths() {
		if err := ctx.Err(); err != nil {
			return
		}
		rec, ok := ledger.Get(path)
		if !ok {
			continue
		}
		want := "synced"
		if origin, here := present[path]; !here {
			// It was pushed from this folder and is not there any more. THE
			// COPY IS NOT TOUCHED -- this is a label, and the row stays in the
			// ledger so a later sweep can tell "gone" from "never seen".
			want = "unavailable"
			// Excluded, inaccessible and unmounted paths are not proof of
			// deletion. Only an authorized lookup that confirms ENOENT is.
			if m.opts.CheckPath != nil && m.opts.CheckPath(path) == nil {
				_, statErr := os.Lstat(path)
				if errors.Is(statErr, fs.ErrNotExist) {
					want = "origin_gone"
				}
			}
		} else if !origin.Unchanged(rec.Stamp) {
			// The origin has moved on from what was last successfully pushed.
			// Reported rather than assumed transient: a push that keeps
			// failing would otherwise leave the copy described as current
			// indefinitely, and the person would have no way to see it.
			want = "stale"
			if rec.LinkState == "conflict" {
				want = "conflict"
			}
		}
		changed := rec.LinkState != want
		stale := now-rec.VerifiedAtUnix >= int64(verifyRefreshInterval/time.Second)
		if !changed && !stale {
			continue
		}
		fileID := rec.FileID
		if fileID == "" {
			resolved, err := m.graph.FileIDAt(ctx, m.workerID, path)
			if err != nil || resolved == "" {
				continue
			}
			fileID = resolved
		}
		if err := m.graph.SetLinkState(ctx, fileID, want); err != nil {
			m.logger.Warn("backup: could not report a file's state", "file", path, "error", err)
			continue
		}
		rec.FileID = fileID
		rec.LinkState = want
		rec.VerifiedAtUnix = now
		ledger.Put(path, rec)
	}
}

// flagAllUnavailable preserves uncertainty when the root is gone or unreadable.
func (m *Manager) flagAllUnavailable(ctx context.Context, watch Watch) {
	ledger := m.ledgerFor(watch.ID)
	for _, path := range ledger.Paths() {
		rec, _ := ledger.Get(path)
		if rec.FileID == "" || rec.LinkState == "unavailable" {
			continue
		}
		if m.graph.SetLinkState(ctx, rec.FileID, "unavailable") == nil {
			rec.LinkState = "unavailable"
			rec.VerifiedAtUnix = time.Now().Unix()
			ledger.Put(path, rec)
		}
	}
	if err := ledger.Save(); err != nil {
		m.logger.Warn("backup: could not save this machine's record", "watch", watch.ID, "error", err)
	}
}

func (m *Manager) report(ctx context.Context, watchID, state string, files int, bytes int64, message string) {
	if err := m.graph.ReportSweep(ctx, SweepReport{
		WatchID:     watchID,
		OriginState: state,
		FilesSeen:   files,
		BytesSeen:   bytes,
		Error:       message,
	}); err != nil {
		m.logger.Warn("backup: could not report a sweep", "watch", watchID, "error", err)
	}
}

func (m *Manager) ledgerFor(watchID string) *Ledger {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l, ok := m.ledgers[watchID]; ok {
		return l
	}
	l := LoadLedger(m.opts.StateDir, watchID)
	m.ledgers[watchID] = l
	return l
}

// sleepFor waits, with a little jitter, and reports whether the wait finished
// rather than the context ending. Jittered for the reason the reconnect
// backoff is: a fleet of machines that all woke at nine o'clock should not all
// start walking at the same instant.
func sleepFor(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	jitter := time.Duration(rand.Int63n(int64(d/4) + 1))
	timer := time.NewTimer(d + jitter)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
