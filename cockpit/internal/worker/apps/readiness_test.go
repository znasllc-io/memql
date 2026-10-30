package apps

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/harness"
)

// checkLog stands in for the real checks. What it answers is set per app, and what
// it records is the only assertion that matters here: when a check ran at all.
type checkLog struct {
	mu      sync.Mutex
	calls   []string
	answers map[string]harness.ReadinessSignal
}

func (l *checkLog) check(_ context.Context, id string, _ Spec) harness.ReadinessSignal {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, id)
	if answer, ok := l.answers[id]; ok {
		return answer
	}
	return harness.ReadinessSignal{State: harness.ReadinessReady, Spent: id == IDClaudeCode}
}

func (l *checkLog) count(id string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, call := range l.calls {
		if call == id {
			n++
		}
	}
	return n
}

// trackerFixture is a tracker on a clock the test moves, with both apps on PATH.
type trackerFixture struct {
	*ReadinessTracker
	now     time.Time
	log     *checkLog
	allowed map[string]bool
	home    string
}

func newTrackerFixture(t *testing.T) *trackerFixture {
	t.Helper()
	f := &trackerFixture{
		now:     time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC),
		log:     &checkLog{answers: map[string]harness.ReadinessSignal{}},
		allowed: map[string]bool{IDClaudeCode: true, IDCodex: true},
		home:    t.TempDir(),
	}
	f.ReadinessTracker = f.build(filepath.Join(t.TempDir(), "readiness.json"))
	return f
}

func (f *trackerFixture) build(statePath string) *ReadinessTracker {
	return NewReadinessTracker(ReadinessOptions{
		Detector: &Detector{
			LookPath:   func(bin string) (string, error) { return "/usr/local/bin/" + bin, nil },
			RunVersion: func(context.Context, string, []string) (string, error) { return "1.0.0", nil },
			RunProbe:   func(context.Context, string, []string) error { return nil },
			Home:       f.home,
		},
		Allowed:   func(id string) bool { return f.allowed[id] },
		Check:     f.log.check,
		Now:       func() time.Time { return f.now },
		StatePath: statePath,
	})
}

func (f *trackerFixture) tick() { f.Tick(context.Background()) }

func (f *trackerFixture) advance(d time.Duration) {
	f.now = f.now.Add(d)
	f.tick()
}

func TestTrackerChecksEachAllowedAppAndNothingElse(t *testing.T) {
	f := newTrackerFixture(t)
	f.allowed[IDCodex] = false
	f.tick()
	if f.log.count(IDClaudeCode) != 1 {
		t.Fatalf("an allowed app was not checked: %v", f.log.calls)
	}
	if f.log.count(IDCodex) != 0 {
		t.Fatalf("no cluster allows this app, so nothing may be spent on it: %v", f.log.calls)
	}
	f.allowed[IDCodex] = true
	f.tick()
	if f.log.count(IDCodex) != 1 {
		t.Fatalf("an allow must reach the next tick: %v", f.log.calls)
	}
}

// A ping costs the owner tokens and Codex's read costs nothing, and the two
// cadences say so.
func TestTrackerChecksOnEachAppsOwnCadence(t *testing.T) {
	f := newTrackerFixture(t)
	f.tick()
	f.advance(4 * time.Minute)
	if f.log.count(IDCodex) != 1 || f.log.count(IDClaudeCode) != 1 {
		t.Fatalf("checked early: %v", f.log.calls)
	}
	f.advance(time.Minute)
	if f.log.count(IDCodex) != 2 {
		t.Fatalf("Codex is free and every 5 minutes: %v", f.log.calls)
	}
	f.advance(54 * time.Minute)
	if f.log.count(IDClaudeCode) != 1 {
		t.Fatalf("pinged before the hour: %v", f.log.calls)
	}
	f.advance(time.Minute)
	if f.log.count(IDClaudeCode) != 2 {
		t.Fatalf("the hourly ping did not run: %v", f.log.calls)
	}
}

func TestTrackerSkipsAppsWithARunningSession(t *testing.T) {
	f := newTrackerFixture(t)
	f.SessionStarted(IDClaudeCode)
	f.tick()
	if f.log.count(IDClaudeCode) != 0 || f.log.count(IDCodex) != 1 {
		t.Fatalf("a running session must suppress only its own app's check: %v", f.log.calls)
	}
	f.SessionEnded(IDClaudeCode)
	f.tick()
	if f.log.count(IDClaudeCode) != 1 {
		t.Fatalf("the check did not resume: %v", f.log.calls)
	}
}

// A busy machine never pings: every real turn is a better check than a ping.
func TestTrackerReusesARealTurnInsteadOfPinging(t *testing.T) {
	f := newTrackerFixture(t)
	f.tick()
	f.now = f.now.Add(50 * time.Minute)
	f.Observe(IDClaudeCode, harness.ReadinessSignal{State: harness.ReadinessReady})
	f.advance(30 * time.Minute)
	if f.log.count(IDClaudeCode) != 1 {
		t.Fatalf("a real turn 30 minutes ago should stand in for this ping: %v", f.log.calls)
	}
	f.advance(30 * time.Minute)
	if f.log.count(IDClaudeCode) != 2 {
		t.Fatalf("an hour after the real turn the ping is due: %v", f.log.calls)
	}
}

func TestTrackerTakesARealTurnsVerdictAtOnce(t *testing.T) {
	f := newTrackerFixture(t)
	f.tick()
	f.Observe(IDClaudeCode, harness.ReadinessSignal{State: harness.ReadinessSignedOut})
	report, ok := f.Report(IDClaudeCode)
	if !ok || report.State != harness.ReadinessSignedOut || !report.CheckedAt.Equal(f.now) {
		t.Fatalf("got %+v", report)
	}
}

// Codex's refusal names no reset the machine can trust, and its read is free, so a
// refused turn asks at once. Claude Code's stream already carried the reset.
func TestTrackerAsksCodexForTheResetARefusedTurnDidNotName(t *testing.T) {
	f := newTrackerFixture(t)
	f.tick()
	reset := f.now.Add(72 * time.Hour)
	f.log.answers[IDCodex] = harness.ReadinessSignal{State: harness.ReadinessOutOfUsage, ResetsAt: reset}
	f.Observe(IDCodex, harness.ReadinessSignal{State: harness.ReadinessOutOfUsage})
	f.tick()
	report, _ := f.Report(IDCodex)
	if f.log.count(IDCodex) != 2 || !report.ResetsAt.Equal(reset) {
		t.Fatalf("no follow-up read: %v %+v", f.log.calls, report)
	}
	f.Observe(IDClaudeCode, harness.ReadinessSignal{State: harness.ReadinessOutOfUsage, ResetsAt: reset})
	f.tick()
	if f.log.count(IDClaudeCode) != 1 {
		t.Fatalf("Claude Code's turn already said when: %v", f.log.calls)
	}
}

func TestTrackerStopsPingingAtTheCeiling(t *testing.T) {
	f := newTrackerFixture(t)
	f.allowed[IDCodex] = false
	for i := 0; i < MaxClaudePingsPerDay+5; i++ {
		f.Recheck(IDClaudeCode)
		f.advance(time.Minute)
	}
	if got := f.log.count(IDClaudeCode); got != MaxClaudePingsPerDay {
		t.Fatalf("pings in a day: got %d, want the ceiling %d", got, MaxClaudePingsPerDay)
	}
	// The window rolls, and the last state stands meanwhile rather than reading
	// as a failure nobody could explain.
	report, _ := f.Report(IDClaudeCode)
	if report.State != harness.ReadinessReady {
		t.Fatalf("the ceiling must not change the answer: %+v", report)
	}
	f.advance(24 * time.Hour)
	if f.log.count(IDClaudeCode) != MaxClaudePingsPerDay+1 {
		t.Fatalf("the ceiling did not roll: %d", f.log.count(IDClaudeCode))
	}
}

func TestTrackerCountsOnlyWhatACheckSpent(t *testing.T) {
	f := newTrackerFixture(t)
	f.allowed[IDCodex] = false
	f.log.answers[IDClaudeCode] = harness.ReadinessSignal{State: harness.ReadinessSignedOut}
	for i := 0; i < MaxClaudePingsPerDay+5; i++ {
		f.Recheck(IDClaudeCode)
		f.advance(time.Minute)
	}
	if got := f.log.count(IDClaudeCode); got != MaxClaudePingsPerDay+5 {
		t.Fatalf("a refused ping costs nothing and must not use up the ceiling: %d", got)
	}
}

// A service that keeps crashing must not ping on every start, which is the one way
// a ping could otherwise run away.
func TestTrackerSurvivesARestartWithoutSpendingAgain(t *testing.T) {
	f := newTrackerFixture(t)
	f.tick()
	first := f.ReadinessTracker
	statePath := first.statePath
	for i := 0; i < 5; i++ {
		f.now = f.now.Add(10 * time.Second)
		f.ReadinessTracker = f.build(statePath)
		f.tick()
	}
	if f.log.count(IDClaudeCode) != 1 {
		t.Fatalf("each restart pinged: %d", f.log.count(IDClaudeCode))
	}
	report, ok := f.Report(IDClaudeCode)
	if !ok || report.State != harness.ReadinessReady {
		t.Fatalf("a restart forgot the answer: %+v", report)
	}
	// The ceiling lives through a restart too.
	raw, err := os.ReadFile(statePath)
	if err != nil || len(raw) == 0 {
		t.Fatalf("nothing was persisted: %v", err)
	}
}

// Credentials never reach the state file: it holds states and times.
func TestTrackerStateFileIsPrivate(t *testing.T) {
	f := newTrackerFixture(t)
	f.tick()
	info, err := os.Stat(f.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
}

func TestTrackerRechecksASignedOutAppWhenItsLoginFileChanges(t *testing.T) {
	f := newTrackerFixture(t)
	f.allowed[IDCodex] = false
	f.log.answers[IDClaudeCode] = harness.ReadinessSignal{State: harness.ReadinessSignedOut}
	f.tick()
	f.advance(2 * time.Minute)
	if f.log.count(IDClaudeCode) != 1 {
		t.Fatalf("rechecked with nothing changed: %d", f.log.count(IDClaudeCode))
	}
	dir := filepath.Join(f.home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f.log.answers[IDClaudeCode] = harness.ReadinessSignal{State: harness.ReadinessReady, Spent: true}
	f.advance(time.Minute)
	report, _ := f.Report(IDClaudeCode)
	if f.log.count(IDClaudeCode) != 2 || report.State != harness.ReadinessReady {
		t.Fatalf("a sign-in in a terminal was not noticed: %d %+v", f.log.count(IDClaudeCode), report)
	}
}

// Any interactive Claude Code rewrites ~/.claude.json, so a working app must not
// spend a ping every time somebody opens a terminal.
func TestTrackerIgnoresLoginFileChangesWhileReady(t *testing.T) {
	f := newTrackerFixture(t)
	f.allowed[IDCodex] = false
	f.tick()
	if err := os.WriteFile(filepath.Join(f.home, ".claude.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f.advance(5 * time.Minute)
	if f.log.count(IDClaudeCode) != 1 {
		t.Fatalf("pinged on a config rewrite: %d", f.log.count(IDClaudeCode))
	}
}

func TestTrackerComesBackAtTheResetEvenIfNoCheckHasRunYet(t *testing.T) {
	f := newTrackerFixture(t)
	reset := f.now.Add(90 * time.Minute)
	f.log.answers[IDClaudeCode] = harness.ReadinessSignal{State: harness.ReadinessOutOfUsage, ResetsAt: reset}
	f.tick()
	report, _ := f.Report(IDClaudeCode)
	if report.State != harness.ReadinessOutOfUsage || !report.ResetsAt.Equal(reset) {
		t.Fatalf("got %+v", report)
	}
	f.SessionStarted(IDClaudeCode)
	f.now = reset.Add(time.Second)
	report, _ = f.Report(IDClaudeCode)
	if report.State != harness.ReadinessUnknown {
		t.Fatalf("a limit that has reset must not keep an app off the machine: %+v", report)
	}
}

func TestTrackerChecksAgainAtTheResetRatherThanTheNextHour(t *testing.T) {
	f := newTrackerFixture(t)
	f.allowed[IDCodex] = false
	reset := f.now.Add(10 * time.Minute)
	f.log.answers[IDClaudeCode] = harness.ReadinessSignal{State: harness.ReadinessOutOfUsage, ResetsAt: reset}
	f.tick()
	f.advance(9 * time.Minute)
	if f.log.count(IDClaudeCode) != 1 {
		t.Fatalf("checked before the reset: %d", f.log.count(IDClaudeCode))
	}
	f.advance(90 * time.Second)
	if f.log.count(IDClaudeCode) != 2 {
		t.Fatalf("did not check at the reset: %d", f.log.count(IDClaudeCode))
	}
}

// On metered overage each ping would be paid for, so pings wait for the reset.
func TestTrackerDoesNotPingThroughMeteredOverage(t *testing.T) {
	f := newTrackerFixture(t)
	f.allowed[IDCodex] = false
	reset := f.now.Add(3 * time.Hour)
	f.log.answers[IDClaudeCode] = harness.ReadinessSignal{State: harness.ReadinessReady, MeteredOverage: true, ResetsAt: reset, Spent: true}
	f.tick()
	f.advance(2 * time.Hour)
	if f.log.count(IDClaudeCode) != 1 {
		t.Fatalf("pinged through overage: %d", f.log.count(IDClaudeCode))
	}
	f.advance(61 * time.Minute)
	if f.log.count(IDClaudeCode) != 2 {
		t.Fatalf("did not resume after the reset: %d", f.log.count(IDClaudeCode))
	}
}

func TestTrackerReportsNothingBeforeTheFirstCheck(t *testing.T) {
	f := newTrackerFixture(t)
	if _, ok := f.Report(IDClaudeCode); ok {
		t.Fatal("no check has run, so there is nothing to report")
	}
}

func TestDetectMergesWhatWasMeasured(t *testing.T) {
	reset := time.Date(2026, 11, 3, 23, 52, 3, 0, time.UTC)
	checked := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	source := readinessSourceFunc(func(id string) (ReadinessReport, bool) {
		switch id {
		case IDClaudeCode:
			return ReadinessReport{State: harness.ReadinessSignedOut, CheckedAt: checked}, true
		case IDCodex:
			return ReadinessReport{State: harness.ReadinessOutOfUsage, CheckedAt: checked, ResetsAt: reset}, true
		}
		return ReadinessReport{}, false
	})
	home := t.TempDir()
	// The file says signed in, as an expired login's file does.
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &Detector{
		LookPath:   func(bin string) (string, error) { return "/usr/local/bin/" + bin, nil },
		RunVersion: func(context.Context, string, []string) (string, error) { return "1.0.0", nil },
		RunProbe:   func(context.Context, string, []string) error { return nil },
		Home:       home,
		Readiness:  source,
	}
	inventory := d.Detect(context.Background(), []string{IDClaudeCode, IDCodex})
	byID := map[string]Info{}
	for _, info := range inventory {
		byID[info.Id] = info
	}
	claude := byID[IDClaudeCode]
	if claude.SignedIn || claude.Readiness != harness.ReadinessSignedOut || !claude.ReadinessCheckedAt.Equal(checked) {
		t.Fatalf("a measured sign-out must beat the file: %+v", claude)
	}
	codex := byID[IDCodex]
	if !codex.SignedIn || codex.Readiness != harness.ReadinessOutOfUsage || !codex.UsageResetsAt.Equal(reset) {
		t.Fatalf("a measured limit proves a login too, even with no file to read: %+v", codex)
	}
}

// A machine with no tracker reports exactly what it always did, which the engine
// reads as unknown.
func TestDetectWithNothingMeasuredKeepsTheFileAnswer(t *testing.T) {
	d := &Detector{
		LookPath:   func(bin string) (string, error) { return "/usr/local/bin/" + bin, nil },
		RunVersion: func(context.Context, string, []string) (string, error) { return "1.0.0", nil },
		RunProbe:   func(context.Context, string, []string) error { return nil },
		Home:       t.TempDir(),
	}
	for _, info := range d.Detect(context.Background(), nil) {
		if info.Readiness != "" || !info.ReadinessCheckedAt.IsZero() {
			t.Fatalf("got %+v", info)
		}
	}
}
