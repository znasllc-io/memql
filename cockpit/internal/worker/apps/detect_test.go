package apps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEnv builds a Detector wired to a temp home and a fixed PATH
// resolution, so none of this needs a real claude or codex.
type fakeEnv struct {
	home     string
	present  map[string]string // binary -> resolved path
	versions map[string]string // resolved path -> version output
	calls    atomic.Int64
	// answers records which subcommand probes a binary answers
	// successfully, keyed by "<resolved path> <arg> <arg>". A path
	// absent from the map is a binary that does not know the
	// subcommand, which is the ordinary state of a Codex older than
	// its app-server.
	answers    map[string]bool
	probeCalls atomic.Int64
	now        time.Time
}

func newFakeEnv(t *testing.T) *fakeEnv {
	t.Helper()
	return &fakeEnv{
		home:     t.TempDir(),
		present:  map[string]string{},
		versions: map[string]string{},
		answers:  map[string]bool{},
		now:      time.Unix(1_700_000_000, 0).UTC(),
	}
}

// answer makes a binary respond successfully to one subcommand probe.
func (f *fakeEnv) answer(path string, args ...string) {
	f.answers[strings.Join(append([]string{path}, args...), " ")] = true
}

// install makes a binary resolvable and gives it a real file on disk, so
// the version cache's size+mtime stamp has something to read.
func (f *fakeEnv) install(t *testing.T, binary, version string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), binary)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	f.present[binary] = path
	f.versions[path] = version
	return path
}

func (f *fakeEnv) writeJSON(t *testing.T, rel string, obj any) {
	t.Helper()
	path := filepath.Join(f.home, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func (f *fakeEnv) detector() *Detector {
	return &Detector{
		Home: f.home,
		Now:  func() time.Time { return f.now },
		LookPath: func(bin string) (string, error) {
			if p, ok := f.present[bin]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		RunVersion: func(_ context.Context, bin string, _ []string) (string, error) {
			f.calls.Add(1)
			if v, ok := f.versions[bin]; ok {
				return v, nil
			}
			return "", errors.New("no version")
		},
		RunProbe: func(_ context.Context, bin string, args []string) error {
			f.probeCalls.Add(1)
			if f.answers[strings.Join(append([]string{bin}, args...), " ")] {
				return nil
			}
			return errors.New("unrecognized subcommand")
		},
	}
}

func find(t *testing.T, got []Info, id string) Info {
	t.Helper()
	for _, a := range got {
		if a.Id == id {
			return a
		}
	}
	t.Fatalf("no entry for %q in %+v", id, got)
	return Info{}
}

// TestDetect_AbsentAppsAreOmitted: an app that is not on PATH produces no
// entry at all. There is nothing to report and nothing an operator can do
// about it, which is different from "installed but blocked".
func TestDetect_AbsentAppsAreOmitted(t *testing.T) {
	env := newFakeEnv(t)
	got := env.detector().Detect(context.Background(), []string{IDClaudeCode, IDCodex})
	if len(got) != 0 {
		t.Fatalf("nothing installed, got %+v", got)
	}
}

// TestDetect_PresentButNotAllowedIsReported is the distinction the issue
// turns on: an app that is present but missing from apps.allow reports
// allowed=false rather than being dropped. Dropping it renders identically
// to "not installed" in the portal, and sends an operator to look for the
// wrong problem.
func TestDetect_PresentButNotAllowedIsReported(t *testing.T) {
	env := newFakeEnv(t)
	env.install(t, "claude", "2.1.4 (Claude Code)")

	got := env.detector().Detect(context.Background(), nil)
	if len(got) != 1 {
		t.Fatalf("want the app reported, got %+v", got)
	}
	entry := find(t, got, IDClaudeCode)
	if entry.Allowed {
		t.Error("an app absent from apps.allow must report allowed=false")
	}
	if entry.Version != "2.1.4 (Claude Code)" {
		t.Errorf("version = %q, want the CLI's own string verbatim", entry.Version)
	}
}

// TestDetect_VersionIsVerbatim: the engine reduces the version to
// major.minor for the routing label, so pre-reducing it here would throw
// away the patch level the portal shows. Only trailing whitespace and
// anything past the first newline are dropped.
func TestDetect_VersionIsVerbatim(t *testing.T) {
	env := newFakeEnv(t)
	env.install(t, "claude", "2.1.4 (Claude Code)\nsome trailing banner\n")
	env.install(t, "codex", "  codex-cli 0.9.1  \n")

	got := env.detector().Detect(context.Background(), nil)
	if v := find(t, got, IDClaudeCode).Version; v != "2.1.4 (Claude Code)" {
		t.Errorf("claude version = %q", v)
	}
	if v := find(t, got, IDCodex).Version; v != "codex-cli 0.9.1" {
		t.Errorf("codex version = %q", v)
	}
}

// TestDetect_UnknownSignedInReportsFalse is the rule the whole epic rests
// on. A machine whose auth state cannot be read reports signed_in=false,
// so the engine derives no routing label and never selects it. A
// best-effort true would commit a plan to a machine that then refuses the
// run, and the failure would name the router rather than the auth state.
func TestDetect_UnknownSignedInReportsFalse(t *testing.T) {
	env := newFakeEnv(t)
	env.install(t, "claude", "2.1.4")
	env.install(t, "codex", "0.9.1")

	got := env.detector().Detect(context.Background(), []string{IDClaudeCode, IDCodex})
	for _, a := range got {
		if a.SignedIn {
			t.Errorf("%s: no auth state on disk must read as signed_in=false", a.Id)
		}
		if a.Subscription != SubscriptionUnknown {
			t.Errorf("%s: subscription = %q, want %q", a.Id, a.Subscription, SubscriptionUnknown)
		}
	}
}

// TestDetect_SignedInFromCredentialsFile covers the Linux/no-keyring
// shape: ~/.claude/.credentials.json with a live access token.
func TestDetect_SignedInFromCredentialsFile(t *testing.T) {
	env := newFakeEnv(t)
	env.install(t, "claude", "2.1.4")
	env.writeJSON(t, ".claude/.credentials.json", map[string]any{
		"claudeAiOauth": map[string]any{"accessToken": "sk-ant-oat-xxx"},
	})

	entry := find(t, env.detector().Detect(context.Background(), []string{IDClaudeCode}), IDClaudeCode)
	if !entry.SignedIn {
		t.Error("a live access token must read as signed in")
	}
	if !entry.Allowed {
		t.Error("apps.allow lists it, so allowed must be true")
	}
}

// TestDetect_SignedInFromAccountRecord covers macOS, where the token
// itself is in the Keychain that this must never read. The account record
// in ~/.claude.json is the only non-prompting signal there is.
func TestDetect_SignedInFromAccountRecord(t *testing.T) {
	env := newFakeEnv(t)
	env.install(t, "claude", "2.1.4")
	env.writeJSON(t, ".claude.json", map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "someone@example.com"},
	})

	if !find(t, env.detector().Detect(context.Background(), nil), IDClaudeCode).SignedIn {
		t.Error("an account record must read as signed in")
	}
}

// TestDetect_CodexAuthShapes: either an API key or an OAuth token pair
// counts as signed in.
func TestDetect_CodexAuthShapes(t *testing.T) {
	t.Run("api key", func(t *testing.T) {
		env := newFakeEnv(t)
		env.install(t, "codex", "0.9.1")
		env.writeJSON(t, ".codex/auth.json", map[string]any{"OPENAI_API_KEY": "sk-xxx"})
		if !find(t, env.detector().Detect(context.Background(), nil), IDCodex).SignedIn {
			t.Error("an API key must read as signed in")
		}
	})
	t.Run("oauth tokens", func(t *testing.T) {
		env := newFakeEnv(t)
		env.install(t, "codex", "0.9.1")
		env.writeJSON(t, ".codex/auth.json", map[string]any{
			"tokens": map[string]any{"access_token": "at-xxx"},
		})
		if !find(t, env.detector().Detect(context.Background(), nil), IDCodex).SignedIn {
			t.Error("an oauth token must read as signed in")
		}
	})
	t.Run("empty file", func(t *testing.T) {
		env := newFakeEnv(t)
		env.install(t, "codex", "0.9.1")
		env.writeJSON(t, ".codex/auth.json", map[string]any{})
		if find(t, env.detector().Detect(context.Background(), nil), IDCodex).SignedIn {
			t.Error("an auth file with no credential must read as NOT signed in")
		}
	})
}

// TestDetect_SubscriptionIsReportedNeverInferred: the value comes from a
// key the app itself wrote, and stays "unknown" when the app said
// nothing. The engine records unknown as billing "unknown", which is the
// honest answer; a value derived from "they have the binary" would be
// recorded as measured.
func TestDetect_SubscriptionIsReportedNeverInferred(t *testing.T) {
	cases := []struct {
		name  string
		claim map[string]any
		want  string
	}{
		{"silent", map[string]any{"accountUuid": "u-1"}, SubscriptionUnknown},
		{"named plan", map[string]any{"accountUuid": "u-1", "subscriptionType": "max"}, SubscriptionPresent},
		{"explicit none", map[string]any{"accountUuid": "u-1", "subscriptionType": "none"}, SubscriptionNone},
		{"explicit free", map[string]any{"accountUuid": "u-1", "subscriptionType": "free"}, SubscriptionNone},
		{"explicit unknown", map[string]any{"accountUuid": "u-1", "subscriptionType": "unknown"}, SubscriptionUnknown},
		{"empty string", map[string]any{"accountUuid": "u-1", "subscriptionType": ""}, SubscriptionUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newFakeEnv(t)
			env.install(t, "claude", "2.1.4")
			env.writeJSON(t, ".claude.json", map[string]any{"oauthAccount": tc.claim})
			got := find(t, env.detector().Detect(context.Background(), nil), IDClaudeCode).Subscription
			if got != tc.want {
				t.Errorf("subscription = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDetect_MalformedStateFileReadsAsCannotTell: a half-written state
// file must not panic, error, or be read optimistically. It reads as
// "cannot tell", which is signed_in=false.
func TestDetect_MalformedStateFileReadsAsCannotTell(t *testing.T) {
	env := newFakeEnv(t)
	env.install(t, "claude", "2.1.4")
	path := filepath.Join(env.home, ".claude.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if find(t, env.detector().Detect(context.Background(), nil), IDClaudeCode).SignedIn {
		t.Error("a malformed state file must not read as signed in")
	}
}

// TestDetect_OrderIsStable: the engine sorts by id anyway, but an
// unstable order here would rewrite the registration row on every beat
// for no actual change.
func TestDetect_OrderIsStable(t *testing.T) {
	env := newFakeEnv(t)
	env.install(t, "codex", "0.9.1")
	env.install(t, "claude", "2.1.4")

	var first []string
	for i := range 25 {
		var ids []string
		for _, a := range env.detector().Detect(context.Background(), nil) {
			ids = append(ids, a.Id)
		}
		if i == 0 {
			first = ids
			continue
		}
		if strings.Join(ids, ",") != strings.Join(first, ",") {
			t.Fatalf("order drifted on run %d: %v vs %v", i, ids, first)
		}
	}
	if strings.Join(first, ",") != IDClaudeCode+","+IDCodex {
		t.Errorf("order = %v, want sorted by id", first)
	}
}

// TestDetect_VersionIsCachedButAuthIsNot pins the cadence bargain. Detect
// runs on every heartbeat; the version comes from a subprocess and is
// reused, while auth state is a file read and is taken fresh -- so
// signing into an app shows up on the NEXT BEAT without forking two
// processes a minute on somebody's laptop forever.
func TestDetect_VersionIsCachedButAuthIsNot(t *testing.T) {
	env := newFakeEnv(t)
	env.install(t, "claude", "2.1.4")
	d := env.detector()

	for range 5 {
		d.Detect(context.Background(), nil)
	}
	if got := env.calls.Load(); got != 1 {
		t.Errorf("version probed %d times across 5 beats, want 1", got)
	}

	// Sign in between beats. The very next beat must see it.
	env.writeJSON(t, ".claude/.credentials.json", map[string]any{
		"claudeAiOauth": map[string]any{"accessToken": "sk-ant-oat-xxx"},
	})
	if !find(t, d.Detect(context.Background(), nil), IDClaudeCode).SignedIn {
		t.Fatal("signing in must be visible on the next beat, not the next reconnect")
	}
	if got := env.calls.Load(); got != 1 {
		t.Errorf("the auth re-read cost a version probe (%d calls)", got)
	}
}

// TestVersion_IsTheInventorysAnswer: the fingerprint's app version comes
// from the same probe and the same cache the inventory reports from.
func TestVersion_IsTheInventorysAnswer(t *testing.T) {
	env := newFakeEnv(t)
	env.install(t, "claude", "2.1.4 (Claude Code)")
	d := env.detector()
	if got := d.Version(context.Background(), IDClaudeCode); got != "2.1.4 (Claude Code)" {
		t.Errorf("Version = %q", got)
	}
	d.Detect(context.Background(), nil)
	if got := env.calls.Load(); got != 1 {
		t.Errorf("the version was probed %d times for one binary, want once", got)
	}
	if got := d.Version(context.Background(), IDCodex); got != "" {
		t.Errorf("an app not on PATH has version %q", got)
	}
	if got := d.Version(context.Background(), "not-an-app"); got != "" {
		t.Errorf("an id outside the closed set has version %q", got)
	}
}

// TestDetect_UpgradeInPlaceInvalidatesTheCache: replacing the binary must
// not leave a stale version on the wire until the TTL expires. The cache
// key carries the binary's size and mtime for exactly this.
func TestDetect_UpgradeInPlaceInvalidatesTheCache(t *testing.T) {
	env := newFakeEnv(t)
	path := env.install(t, "claude", "2.1.4")
	d := env.detector()

	if v := find(t, d.Detect(context.Background(), nil), IDClaudeCode).Version; v != "2.1.4" {
		t.Fatalf("version = %q", v)
	}

	// Upgrade in place, well inside the TTL.
	env.versions[path] = "2.2.0"
	if err := os.WriteFile(path, []byte("#!/bin/sh\n# upgraded\n"), 0o755); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := os.Chtimes(path, env.now.Add(time.Hour), env.now.Add(time.Hour)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if v := find(t, d.Detect(context.Background(), nil), IDClaudeCode).Version; v != "2.2.0" {
		t.Errorf("version = %q after an in-place upgrade, want 2.2.0", v)
	}
}

// TestDetect_VersionProbeFailureStillReportsTheApp: a binary that will
// not answer --version is still installed. The engine reads an
// unparseable version as "any version" rather than as absent, and the
// failure is not cached -- a half-finished install should resolve itself
// on the next beat, not in five minutes.
func TestDetect_VersionProbeFailureStillReportsTheApp(t *testing.T) {
	env := newFakeEnv(t)
	path := env.install(t, "claude", "2.1.4")
	delete(env.versions, path)
	d := env.detector()

	entry := find(t, d.Detect(context.Background(), nil), IDClaudeCode)
	if entry.Version != "" {
		t.Errorf("version = %q, want empty", entry.Version)
	}
	env.versions[path] = "2.1.4"
	if v := find(t, d.Detect(context.Background(), nil), IDClaudeCode).Version; v != "2.1.4" {
		t.Errorf("a failed probe was cached: version = %q on the next beat", v)
	}
}

// TestTruncate_MatchesTheEngineBound keeps the portal showing the same
// string the cockpit logged, rather than a longer one clipped in transit.
func TestTruncate_MatchesTheEngineBound(t *testing.T) {
	long := strings.Repeat("v", MaxFieldLen+50)
	if got := Truncate(long); len(got) != MaxFieldLen {
		t.Errorf("len = %d, want %d", len(got), MaxFieldLen)
	}
	if got := Truncate("2.1.4"); got != "2.1.4" {
		t.Errorf("a short value was altered: %q", got)
	}
}

// TestDetect_ClaudeCodeDescriptorNeedsNoProbe. Claude Code has exactly
// one protocol, so its descriptor is a constant. Probing for it would
// fork a subprocess on every beat to answer a question that is written
// down in Specs().
func TestDetect_ClaudeCodeDescriptorNeedsNoProbe(t *testing.T) {
	env := newFakeEnv(t)
	env.install(t, "claude", "2.1.4")

	entry := find(t, env.detector().Detect(context.Background(), nil), IDClaudeCode)
	if entry.Harness != HarnessClaudeHeadless {
		t.Errorf("harness = %q, want %q", entry.Harness, HarnessClaudeHeadless)
	}
	if !entry.StructuredResult || !entry.FollowUps {
		t.Errorf("descriptor = %+v, want both capabilities", entry)
	}
	if got := env.probeCalls.Load(); got != 0 {
		t.Errorf("claude-code cost %d harness probes, want 0", got)
	}
}

// TestDetect_CodexHarnessComesFromThisMachine is the reason the harness
// is REPORTED rather than derived from the app id.
//
// Two harnesses answer to the id `codex`, and only this machine can see
// which one its binary has. An engine that inferred the protocol from the
// id would be right on half the fleet.
func TestDetect_CodexHarnessComesFromThisMachine(t *testing.T) {
	t.Run("app-server present", func(t *testing.T) {
		env := newFakeEnv(t)
		path := env.install(t, "codex", "0.9.1")
		env.answer(path, "app-server", "--help")

		entry := find(t, env.detector().Detect(context.Background(), nil), IDCodex)
		if entry.Harness != HarnessCodexAppServer {
			t.Errorf("harness = %q, want %q", entry.Harness, HarnessCodexAppServer)
		}
		if !entry.StructuredResult {
			t.Error("the app-server returns a schema'd result; the descriptor must say so or the app door is shut for structured calls")
		}
		if !entry.FollowUps {
			t.Error("the app-server continues a thread, which is what a follow-up is")
		}
	})

	t.Run("app-server absent", func(t *testing.T) {
		env := newFakeEnv(t)
		env.install(t, "codex", "0.7.0")

		entry := find(t, env.detector().Detect(context.Background(), nil), IDCodex)
		if entry.Harness != HarnessCodexMCP {
			t.Errorf("harness = %q, want the fallback %q", entry.Harness, HarnessCodexMCP)
		}
		if entry.StructuredResult {
			t.Error("the mcp-server tool pair returns a transcript, not a schema'd answer")
		}
		if !entry.FollowUps {
			t.Error("codex-reply continues a thread by id")
		}
	})
}

// TestDetect_HarnessProbeFailsTowardTheFallback. A probe that hangs, is
// killed by its timeout, or fails for any reason the cockpit cannot read
// reports the FALLBACK. Both directions of the error cost something, and
// they do not cost the same: driving an app-server-capable Codex through
// the mcp-server tools loses usage numbers, while driving an old Codex
// through a protocol it does not have kills the session at Start, after
// the engine has committed a turn to this machine.
func TestDetect_HarnessProbeFailsTowardTheFallback(t *testing.T) {
	env := newFakeEnv(t)
	path := env.install(t, "codex", "0.9.1")
	// The binary HAS the app-server, but the probe cannot establish it.
	env.answer(path, "app-server", "--help")
	d := env.detector()
	d.RunProbe = func(ctx context.Context, _ string, _ []string) error {
		return context.DeadlineExceeded
	}

	entry := find(t, d.Detect(context.Background(), nil), IDCodex)
	if entry.Harness != HarnessCodexMCP {
		t.Errorf("harness = %q; a probe that cannot tell must not claim the app-server", entry.Harness)
	}
}

// TestDetect_BlockedAppStillCarriesItsDescriptor. An app present but
// missing from apps.allow is REPORTED with allowed=false rather than
// omitted, and a descriptor for a blocked app is still a descriptor: the
// portal can then say "present, blocked, would be driven through
// codex-app-server" instead of rendering it identically to "not
// installed".
func TestDetect_BlockedAppStillCarriesItsDescriptor(t *testing.T) {
	env := newFakeEnv(t)
	path := env.install(t, "codex", "0.9.1")
	env.answer(path, "app-server", "--help")

	entry := find(t, env.detector().Detect(context.Background(), nil), IDCodex)
	if entry.Allowed {
		t.Fatal("precondition: apps.allow is empty, so this app is blocked")
	}
	if entry.Harness != HarnessCodexAppServer {
		t.Errorf("harness = %q on a blocked app, want it reported anyway", entry.Harness)
	}
}

// TestDetect_HarnessProbeIsCachedInBothDirections pins the cadence
// bargain for the second subprocess this package forks.
//
// Unlike the version probe, a FAILED harness probe is an answer -- this
// binary has no app-server -- rather than "cannot tell", so both outcomes
// are cached. Re-forking `codex app-server --help` on a 15-second beat
// forever, to re-learn that a Codex from last year is still a Codex from
// last year, is exactly the churn the TTL exists to prevent.
func TestDetect_HarnessProbeIsCachedInBothDirections(t *testing.T) {
	for _, tc := range []struct {
		name        string
		hasAppSrv   bool
		wantHarness string
	}{
		{"yes is cached", true, HarnessCodexAppServer},
		{"no is cached", false, HarnessCodexMCP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newFakeEnv(t)
			path := env.install(t, "codex", "0.9.1")
			if tc.hasAppSrv {
				env.answer(path, "app-server", "--help")
			}
			d := env.detector()

			for range 5 {
				if h := find(t, d.Detect(context.Background(), nil), IDCodex).Harness; h != tc.wantHarness {
					t.Fatalf("harness = %q, want %q", h, tc.wantHarness)
				}
			}
			if got := env.probeCalls.Load(); got != 1 {
				t.Errorf("harness probed %d times across 5 beats, want 1", got)
			}
		})
	}
}

// TestDetect_UpgradingCodexInPlaceReprobesTheHarness. Installing a Codex
// that has the app-server must not leave the fallback on the wire until
// the TTL expires -- the whole point of the upgrade is that the operator
// did something and expects it to take. The cache key carries the
// binary's size and mtime for exactly this.
func TestDetect_UpgradingCodexInPlaceReprobesTheHarness(t *testing.T) {
	env := newFakeEnv(t)
	path := env.install(t, "codex", "0.7.0")
	d := env.detector()

	if h := find(t, d.Detect(context.Background(), nil), IDCodex).Harness; h != HarnessCodexMCP {
		t.Fatalf("harness = %q, want the fallback before the upgrade", h)
	}

	// Upgrade in place, well inside the TTL.
	env.answer(path, "app-server", "--help")
	env.versions[path] = "0.9.1"
	if err := os.WriteFile(path, []byte("#!/bin/sh\n# upgraded\n"), 0o755); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := os.Chtimes(path, env.now.Add(time.Hour), env.now.Add(time.Hour)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if h := find(t, d.Detect(context.Background(), nil), IDCodex).Harness; h != HarnessCodexAppServer {
		t.Errorf("harness = %q after an in-place upgrade, want %q", h, HarnessCodexAppServer)
	}
}

// TestResolveSpec_IsTheSameAnswerTheInventoryReports. The app-session
// runner needs the harness for ONE app at the moment it starts a session,
// and it must be the same answer the registration carried -- a second
// implementation could disagree with the first, and nothing on the
// machine would say which one lied.
func TestResolveSpec_IsTheSameAnswerTheInventoryReports(t *testing.T) {
	env := newFakeEnv(t)
	path := env.install(t, "codex", "0.9.1")
	env.answer(path, "app-server", "--help")
	d := env.detector()

	reported := find(t, d.Detect(context.Background(), nil), IDCodex)
	spec, ok := d.ResolveSpec(context.Background(), IDCodex)
	if !ok {
		t.Fatal("codex is installed, so ResolveSpec must find it")
	}
	if spec.Harness != reported.Harness || spec.StructuredResult != reported.StructuredResult || spec.FollowUps != reported.FollowUps {
		t.Errorf("ResolveSpec = %+v, inventory reported %+v", spec, reported)
	}
	// And it reuses the inventory's probe rather than forking its own.
	if got := env.probeCalls.Load(); got != 1 {
		t.Errorf("resolving cost %d probes in total, want 1", got)
	}
}

// TestResolveSpec_NamesTheBinaryItResolved. The session starts the path
// this lookup found, not the bare name: a second lookup at exec time is a
// second answer, and under a LaunchAgent's PATH it was a different one --
// the inventory said "claude-code" while the exec could find no claude.
func TestResolveSpec_NamesTheBinaryItResolved(t *testing.T) {
	env := newFakeEnv(t)
	path := env.install(t, "claude", "2.1.283 (Claude Code)")
	spec, ok := env.detector().ResolveSpec(context.Background(), IDClaudeCode)
	if !ok {
		t.Fatal("claude is installed, so ResolveSpec must find it")
	}
	if spec.Binary != path {
		t.Errorf("Binary = %q, want the resolved %q", spec.Binary, path)
	}
	if floor, _ := SpecFor(IDClaudeCode); floor.Binary != "claude" {
		t.Errorf("the closed set's own spec must keep the bare name, got %q", floor.Binary)
	}
}

// TestResolveSpec_FalseMeansDoNotStartASession. An unknown id and a
// binary that is not on PATH are the same answer to the only question the
// caller has, and collapsing them keeps a caller from starting a session
// against an app this machine cannot run.
func TestResolveSpec_FalseMeansDoNotStartASession(t *testing.T) {
	env := newFakeEnv(t)
	d := env.detector()
	if _, ok := d.ResolveSpec(context.Background(), "not-an-app"); ok {
		t.Error("an id outside the closed set must not resolve")
	}
	if _, ok := d.ResolveSpec(context.Background(), IDCodex); ok {
		t.Error("codex is not on PATH here, so it must not resolve")
	}
}
