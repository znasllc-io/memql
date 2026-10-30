package apps

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/harness"
)

// readiness.go keeps what this machine last measured about each app, and decides
// when to measure again (harness/readiness.go does the measuring).
//
// THE CADENCE IS THE COST. Codex's read spends nothing, so it runs every five
// minutes and buying a reset shows within that. A Claude Code ping spends about 600
// tokens of the owner's plan, so it runs hourly on an idle machine, at most about
// 14,400 tokens a day. Hourly is enough because an hour-stale "ready" costs little:
// the first real call on a lapsed login fails in milliseconds for no tokens, the
// engine moves it to the next app, and this tracker learns from that turn at once.
//
// A REAL TURN BEATS A PING. Every turn reports what it found (TurnResult.Readiness)
// and counts as a check, and no check runs while that app has a session, so a busy
// machine never pings at all.
//
// THE SPEND IS BOUNDED BY A CEILING THAT SURVIVES A RESTART. A trigger that fires
// too often is the way a ping could run away, and a service crash-looping is the
// worst such trigger, so the times and the answer live in a file: a restart picks
// up where it left off rather than checking again. Only a check that may have spent
// counts, because a refused ping is free and a signed-out machine re-checks often.
//
// WHAT IS NOT CHECKED. An app no cluster allows: the machine's owner has not agreed
// to MemQL driving it, so nothing is spent on it.
const (
	// CodexCheckInterval is how often Codex's free read runs, whatever it said.
	CodexCheckInterval = 5 * time.Minute
	// ClaudePingInterval is how often an idle, working Claude Code is pinged.
	ClaudePingInterval = time.Hour
	// ClaudeRecheckInterval is how often a Claude Code that is not known to be
	// working is asked again. Not spending is what lets it be shorter: a signed-out
	// app answers from `auth status`, and a refused ping is free.
	ClaudeRecheckInterval = 15 * time.Minute
	// MaxClaudePingsPerDay is the ceiling on pings that may have spent, in any
	// rolling 24 hours. Above the hourly cadence so sign-ins and restarts fit
	// inside it, and low enough that nothing can spend more than about 18,000
	// tokens a day on one machine.
	MaxClaudePingsPerDay = 30

	// readinessMaxAge is how long an answer read back from the state file is
	// believed. Past it the machine has been off, and it measures again.
	readinessMaxAge = 24 * time.Hour
	// loginChangeFloor bounds how often a changing login file can trigger a check.
	loginChangeFloor = time.Minute
	// readinessTick is how often the tracker looks at what is due.
	readinessTick = 30 * time.Second
	// readinessFirstDelay lets the worker connect before the first check, so a
	// machine starting up is not reporting a stale inventory while it probes.
	readinessFirstDelay = 10 * time.Second
	// readinessCheckTimeout is the watchdog on one check, covering every process
	// it runs. A ping took under 3 seconds when measured.
	readinessCheckTimeout = 90 * time.Second
)

// ReadinessReport is what this machine last measured about one app.
type ReadinessReport struct {
	State     string
	CheckedAt time.Time
	// ResetsAt is when the limit behind out_of_usage resets, zero when unknown.
	ResetsAt time.Time
}

// ReadinessSource answers for Detect what has been measured about an app. ok is
// false when nothing has been, which is what a Cockpit with no tracker reports.
type ReadinessSource interface {
	Report(id string) (ReadinessReport, bool)
}

type readinessSourceFunc func(id string) (ReadinessReport, bool)

func (f readinessSourceFunc) Report(id string) (ReadinessReport, bool) { return f(id) }

// ReadinessOptions builds a tracker. Every field is injectable so the cadence and
// the ceiling can be tested on a clock the test moves, with no app installed.
type ReadinessOptions struct {
	Logger *slog.Logger
	// Detector resolves each app's binary and harness, sharing the inventory's
	// cached probes.
	Detector *Detector
	// Allowed reports whether AT LEAST ONE cluster's policy allows the app. Nil
	// allows nothing, the rest of the worker's posture.
	Allowed func(id string) bool
	// Check measures one app. Nil is the real checks over Launch.
	Check func(ctx context.Context, id string, spec Spec) harness.ReadinessSignal
	// Launch is the supervisor the real checks' processes come from.
	Launch harness.LaunchFunc
	Now    func() time.Time
	// StatePath is the file the answers and the ceiling survive a restart in.
	// Empty keeps them in memory, for a build with no state directory.
	StatePath string
}

// readinessEntry is one app's state. The exported fields are the state file.
type readinessEntry struct {
	State          string      `json:"state,omitempty"`
	CheckedAt      time.Time   `json:"checkedAt,omitempty"`
	ResetsAt       time.Time   `json:"resetsAt,omitempty"`
	MeteredOverage bool        `json:"meteredOverage,omitempty"`
	Spends         []time.Time `json:"spends,omitempty"`

	sessions     int
	recheck      bool
	stamp        string
	loginChanged bool
	ceilingAt    time.Time
}

// ReadinessTracker keeps the answers and runs the checks, one at a time, for the
// whole machine: an app's login and plan are facts about the machine, whichever
// cluster asks. Safe for concurrent use.
type ReadinessTracker struct {
	logger    *slog.Logger
	detector  *Detector
	allowed   func(id string) bool
	check     func(ctx context.Context, id string, spec Spec) harness.ReadinessSignal
	now       func() time.Time
	statePath string
	wake      chan struct{}

	mu      sync.Mutex
	entries map[string]*readinessEntry
}

// NewReadinessTracker builds a tracker and reads back what it last knew.
func NewReadinessTracker(opts ReadinessOptions) *ReadinessTracker {
	t := &ReadinessTracker{
		logger:    opts.Logger,
		detector:  opts.Detector,
		allowed:   opts.Allowed,
		check:     opts.Check,
		now:       opts.Now,
		statePath: opts.StatePath,
		wake:      make(chan struct{}, 1),
		entries:   map[string]*readinessEntry{},
	}
	if t.logger == nil {
		t.logger = slog.Default()
	}
	if t.now == nil {
		t.now = time.Now
	}
	if t.detector == nil {
		t.detector = &Detector{}
	}
	if t.allowed == nil {
		t.allowed = func(string) bool { return false }
	}
	if t.check == nil {
		t.check = realReadinessCheck(opts.Launch)
	}
	t.load()
	return t
}

// UseStatePath names the state file, once the worker knows its state directory,
// and reads back what it last knew. Called before Run.
func (t *ReadinessTracker) UseStatePath(path string, logger *slog.Logger) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.statePath = path
	if logger != nil {
		t.logger = logger
	}
	t.load()
}

// MeasureNow runs one app's real check at once, for a person asking at a terminal.
// It answers to no cadence and no ceiling: they asked, and it spends a ping.
func MeasureNow(ctx context.Context, id string, spec Spec, launch harness.LaunchFunc) harness.ReadinessSignal {
	return realReadinessCheck(launch)(ctx, id, spec)
}

// realReadinessCheck measures one app in an empty directory of its own, removed
// afterwards, in the worker's own environment: Claude Code reads its Keychain entry
// by $USER, and an environment without it reads as signed out on macOS even when
// signed in (measured on 2.1.304).
func realReadinessCheck(launch harness.LaunchFunc) func(context.Context, string, Spec) harness.ReadinessSignal {
	return func(ctx context.Context, id string, spec Spec) harness.ReadinessSignal {
		if launch == nil {
			return harness.ReadinessSignal{State: harness.ReadinessUnknown, Detail: "no process supervisor to check with"}
		}
		dir, err := os.MkdirTemp("", "memql-app-check-")
		if err != nil {
			return harness.ReadinessSignal{State: harness.ReadinessUnknown, Detail: "no directory to check in: " + err.Error()}
		}
		defer os.RemoveAll(dir)
		checkSpec := harness.CheckSpec{Binary: spec.Binary, Dir: dir, Launch: launch, Environ: os.Environ()}
		switch id {
		case IDClaudeCode:
			return harness.CheckClaudeCode(ctx, checkSpec)
		case IDCodex:
			if spec.Harness != HarnessCodexAppServer {
				return harness.ReadinessSignal{State: harness.ReadinessUnknown, Detail: "this Codex has no app-server to ask"}
			}
			return harness.CheckCodex(ctx, checkSpec)
		}
		return harness.ReadinessSignal{State: harness.ReadinessUnknown}
	}
}

// Run checks whatever is due until ctx ends. A worker runs one, for the machine.
func (t *ReadinessTracker) Run(ctx context.Context) {
	if t == nil {
		return
	}
	first := time.NewTimer(readinessFirstDelay)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	ticker := time.NewTicker(readinessTick)
	defer ticker.Stop()
	for {
		t.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-t.wake:
		}
	}
}

// Tick runs every check that is due now, one after another. Exported for the
// worker's report command and the tests.
func (t *ReadinessTracker) Tick(ctx context.Context) {
	for _, spec := range Specs() {
		if ctx.Err() != nil {
			return
		}
		if !t.due(spec.ID) {
			continue
		}
		resolved, ok := t.detector.ResolveSpec(ctx, spec.ID)
		if !ok {
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, readinessCheckTimeout)
		signal := t.check(checkCtx, spec.ID, resolved)
		cancel()
		if ctx.Err() != nil {
			// The worker is stopping and killed the check. That says nothing
			// about the app.
			return
		}
		t.record(spec.ID, signal)
	}
}

func (t *ReadinessTracker) entry(id string) *readinessEntry {
	e, ok := t.entries[id]
	if !ok {
		e = &readinessEntry{}
		t.entries[id] = e
	}
	return e
}

// due decides whether a check should run now. It takes no process and no network.
func (t *ReadinessTracker) due(id string) bool {
	if !t.allowed(id) {
		return false
	}
	now := t.now()
	stamp := loginStamp(id, t.detector.home())

	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entry(id)
	if e.stamp != "" && stamp != e.stamp {
		e.loginChanged = true
	}
	e.stamp = stamp
	if e.sessions > 0 {
		// The session will report what it finds, and the check's process would
		// compete with it for the same login.
		return false
	}
	if id == IDClaudeCode && t.overCeiling(e, now) {
		if e.ceilingAt.IsZero() {
			e.ceilingAt = now
			t.logger.Warn("app readiness: daily ping ceiling reached; keeping the last answer until the window rolls",
				"app", id, "ceiling", MaxClaudePingsPerDay)
		}
		return false
	}
	e.ceilingAt = time.Time{}
	if e.recheck {
		return true
	}
	if e.CheckedAt.IsZero() {
		return true
	}
	// A sign-in in a terminal writes no row and tells nobody. It does change this
	// file, and for a signed-out app that is worth asking about at once. Not for a
	// working one: every interactive Claude Code rewrites ~/.claude.json, and a ping
	// each time would spend the owner's plan on their own terminal.
	//
	// The change stays remembered until a check answers it. The floor delays the
	// question and does not cancel it: a sign-in finished seconds after a check would
	// otherwise read signed out until the next cadence.
	if e.loginChanged && e.State == harness.ReadinessSignedOut && now.Sub(e.CheckedAt) >= loginChangeFloor {
		return true
	}
	return !now.Before(nextCheck(id, e))
}

// nextCheck is when this app should next be measured, from what it last said.
func nextCheck(id string, e *readinessEntry) time.Time {
	interval := CodexCheckInterval
	if id == IDClaudeCode {
		interval = ClaudeRecheckInterval
		if e.State == harness.ReadinessReady || e.State == harness.ReadinessOutOfUsage {
			interval = ClaudePingInterval
		}
	}
	next := e.CheckedAt.Add(interval)
	if e.State == harness.ReadinessOutOfUsage && !e.ResetsAt.IsZero() && e.ResetsAt.Before(next) && e.ResetsAt.After(e.CheckedAt) {
		// The answer is expected to change here, so ask here.
		next = e.ResetsAt
	}
	if e.MeteredOverage && e.ResetsAt.After(next) {
		// Each ping past the plan would be paid for.
		next = e.ResetsAt
	}
	return next
}

// overCeiling prunes the spend log and reports whether it is full.
func (t *ReadinessTracker) overCeiling(e *readinessEntry, now time.Time) bool {
	kept := e.Spends[:0]
	for _, at := range e.Spends {
		if now.Sub(at) < 24*time.Hour {
			kept = append(kept, at)
		}
	}
	e.Spends = kept
	return len(e.Spends) >= MaxClaudePingsPerDay
}

// record files a check's answer.
func (t *ReadinessTracker) record(id string, signal harness.ReadinessSignal) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entry(id)
	e.recheck = false
	e.loginChanged = false
	now := t.now()
	if signal.Spent {
		e.Spends = append(e.Spends, now)
	}
	t.apply(id, e, signal, now)
}

// Observe files what a real turn said about the app. A turn that said nothing
// passes nil and changes nothing: a tool crash is no statement about a login.
//
// A Codex usage refusal names its reset only in a sentence with no time zone, and
// Codex's own read is free and exact, so that one read follows at once.
func (t *ReadinessTracker) Observe(id string, signal harness.ReadinessSignal) {
	if t == nil || signal.State == "" || signal.State == harness.ReadinessUnknown {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entry(id)
	t.apply(id, e, signal, t.now())
	if id == IDCodex && signal.State == harness.ReadinessOutOfUsage && signal.ResetsAt.IsZero() {
		e.recheck = true
	}
}

func (t *ReadinessTracker) apply(id string, e *readinessEntry, signal harness.ReadinessSignal, now time.Time) {
	if e.State != signal.State {
		t.logger.Info("app readiness changed", "app", id, "from", e.State, "to", signal.State,
			"resets_at", signal.ResetsAt, "detail", signal.Detail)
	}
	e.State = signal.State
	e.CheckedAt = now
	e.ResetsAt = signal.ResetsAt
	e.MeteredOverage = signal.MeteredOverage
	t.save()
}

// Recheck asks for a check on the next tick, whatever the cadence says: a sign-in
// just finished. It still answers to the ceiling.
func (t *ReadinessTracker) Recheck(id string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.entry(id).recheck = true
	t.mu.Unlock()
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// SessionStarted and SessionEnded bracket a session of an app, on any cluster.
func (t *ReadinessTracker) SessionStarted(id string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entry(id).sessions++
}

func (t *ReadinessTracker) SessionEnded(id string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.entry(id); e.sessions > 0 {
		e.sessions--
	}
}

// Report implements ReadinessSource.
//
// A LIMIT THAT HAS RESET READS AS UNKNOWN even before a check confirms it, because
// the check may be held off by a running session or the ceiling, and the engine has
// no clock of its own for this: a stale "out of usage" would keep a working app off
// the machine with nothing left to clear it.
func (t *ReadinessTracker) Report(id string) (ReadinessReport, bool) {
	if t == nil {
		return ReadinessReport{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[id]
	if !ok || e.State == "" {
		return ReadinessReport{}, false
	}
	report := ReadinessReport{State: e.State, CheckedAt: e.CheckedAt, ResetsAt: e.ResetsAt}
	if e.State != harness.ReadinessOutOfUsage {
		report.ResetsAt = time.Time{}
	} else if !e.ResetsAt.IsZero() && !t.now().Before(e.ResetsAt) {
		report.State, report.ResetsAt = harness.ReadinessUnknown, time.Time{}
	}
	return report, true
}

type readinessFile struct {
	Apps map[string]*readinessEntry `json:"apps"`
}

func (t *ReadinessTracker) load() {
	if t.statePath == "" {
		return
	}
	raw, err := os.ReadFile(t.statePath)
	if err != nil {
		return
	}
	var saved readinessFile
	if json.Unmarshal(raw, &saved) != nil {
		return
	}
	now := t.now()
	for id, e := range saved.Apps {
		if !IsKnownID(id) || e == nil {
			continue
		}
		if now.Sub(e.CheckedAt) > readinessMaxAge {
			e.State, e.CheckedAt, e.ResetsAt, e.MeteredOverage = "", time.Time{}, time.Time{}, false
		}
		t.entries[id] = e
	}
}

// save keeps the state file current. The caller holds mu. A write that fails costs
// the restart protection and nothing else, so it is logged and work goes on.
func (t *ReadinessTracker) save() {
	if t.statePath == "" {
		return
	}
	raw, err := json.Marshal(readinessFile{Apps: t.entries})
	if err == nil {
		err = os.MkdirAll(filepath.Dir(t.statePath), 0o700)
	}
	if err == nil {
		tmp := t.statePath + ".tmp"
		if err = os.WriteFile(tmp, raw, 0o600); err == nil {
			err = os.Rename(tmp, t.statePath)
		}
	}
	if err != nil {
		t.logger.Warn("app readiness: could not save the state file", "error", err)
	}
}

// loginStamp identifies the files a sign-in writes, by size and time only. Their
// contents are credentials and are not read here.
func loginStamp(id, home string) string {
	if strings.TrimSpace(home) == "" {
		return "-"
	}
	var paths []string
	switch id {
	case IDClaudeCode:
		paths = []string{filepath.Join(home, ".claude", ".credentials.json"), filepath.Join(home, ".claude.json")}
	case IDCodex:
		paths = []string{filepath.Join(home, ".codex", "auth.json")}
	}
	stamp := "-"
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil {
			stamp += "|" + info.ModTime().UTC().Format(time.RFC3339Nano) + "|" + itoa(info.Size())
		}
	}
	return stamp
}
