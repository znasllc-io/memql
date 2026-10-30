package apps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/harness"
	"github.com/znasllc-io/memql/cockpit/internal/worker/processprobe"
)

// Detector reports the local app inventory. Safe for concurrent use; the
// worker calls Detect from the heartbeat loop and from Register.
//
// Every field is injectable so the whole thing is testable without a real
// claude or codex on PATH -- which matters, because CI has neither and a
// detector that could only be exercised by hand is a detector nobody
// exercises.
type Detector struct {
	// LookPath resolves a binary. Defaults to exec.LookPath.
	LookPath func(string) (string, error)
	// RunVersion asks an app for its own version string. Defaults to a
	// bounded exec of the resolved binary.
	RunVersion func(ctx context.Context, bin string, args []string) (string, error)
	// RunProbe asks whether an app answers a subcommand at all. Only the
	// error matters -- nil is yes -- because the question is "does this
	// binary have `app-server`", not "what did it say". Defaults to a
	// bounded exec that discards the output.
	RunProbe func(ctx context.Context, bin string, args []string) error
	// Home is where the apps keep their own state. Defaults to $HOME.
	Home string
	// Now defaults to time.Now.
	Now func() time.Time
	// VersionTTL is how long a probed version is reused. Defaults to
	// DefaultVersionTTL.
	VersionTTL time.Duration
	// Readiness is what has been measured about each app (readiness.go). Nil
	// reports only what the files say.
	Readiness ReadinessSource

	mu           sync.Mutex
	cache        map[string]versionEntry
	harnessCache map[string]harnessEntry
}

// DefaultVersionTTL bounds how stale a cached version string may be.
//
// WHY CACHE AT ALL. Detect runs on EVERY heartbeat -- the engine applies
// an inventory change to its live registry immediately, so signing into
// Claude Code makes this machine selectable on the next beat rather than
// the next reconnect. At a 15s beat, an uncached probe would fork two
// processes a minute forever on somebody's laptop, to answer a question
// whose answer changes when they run an installer.
//
// So the SLOW half is cached and the FAST half never is: the version and
// the harness probe come from subprocesses and are reused; presence and
// auth state are filesystem reads and are taken fresh every beat. That
// keeps the property the cadence exists for -- a sign-in shows up on the
// next beat -- without the process churn.
//
// It bounds the harness probe as well, under the version's name. Both
// answer "what did the operator install", and two probes allowed to go
// stale at different rates would report a Codex version read at one
// moment beside a Codex harness read at another.
//
// The cache key includes the binary's size and mtime, so an in-place
// upgrade invalidates immediately rather than waiting out the TTL.
const DefaultVersionTTL = 5 * time.Minute

// probeTimeout bounds one subprocess this package forks -- a `--version`
// call or a harness probe. An app that hangs on its own version flag, or
// one whose `app-server --help` decides to start a server instead of
// printing anything, must not wedge the heartbeat loop behind it. The
// timeout's failure direction is the safe one in both cases: no version,
// and the fallback harness.
const probeTimeout = 5 * time.Second

// probeWaitDelay is how long a version probe that has exited may still
// hold its output open (runVersion).
const probeWaitDelay = 500 * time.Millisecond

type versionEntry struct {
	version string
	stamp   string
	at      time.Time
}

// harnessEntry caches whether a binary earned its harness upgrade.
//
// BOTH OUTCOMES ARE CACHED, which is where this differs from the version
// probe. A failed version call means "cannot tell" and must be retried,
// so a half-finished install resolves on the next beat. A failed harness
// probe is an ANSWER -- this binary has no app-server -- and re-forking
// `codex app-server --help` every fifteen seconds forever, to re-learn
// that last year's Codex is still last year's Codex, is exactly the churn
// the TTL exists to prevent. An operator who upgrades changes the binary,
// and the stamp key invalidates immediately.
type harnessEntry struct {
	upgraded bool
	stamp    string
	at       time.Time
}

func (d *Detector) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Detector) home() string {
	if d.Home != "" {
		return d.Home
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("HOME")
}

func (d *Detector) lookPath(bin string) (string, error) {
	if d.LookPath != nil {
		return d.LookPath(bin)
	}
	return exec.LookPath(bin)
}

func (d *Detector) runVersion(ctx context.Context, bin string, args []string) (string, error) {
	if d.RunVersion != nil {
		return d.RunVersion(ctx, bin, args)
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	processprobe.Configure(cmd)
	defer processprobe.Terminate(cmd)
	// WaitDelay bounds the wait for the OUTPUT as well as the exit. An app
	// behind a version-manager shim can leave a child holding stdout open
	// after the probe itself has answered, and Output would otherwise wait
	// on that pipe long past the deadline -- in front of a session's start,
	// now that the fingerprint asks this too.
	cmd.WaitDelay = probeWaitDelay
	out, err := cmd.Output()
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return "", err
	}
	return string(out), nil
}

func (d *Detector) runProbe(ctx context.Context, bin string, args []string) error {
	if d.RunProbe != nil {
		return d.RunProbe(ctx, bin, args)
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	// Stdout and Stderr stay nil, which sends both to the null device.
	// The output is not the answer -- a help text and a silent success
	// are the same yes -- and leaving a pipe nobody drains is how a
	// chatty app blocks on a full buffer and takes the heartbeat with it.
	cmd := exec.CommandContext(ctx, bin, args...)
	processprobe.Configure(cmd)
	defer processprobe.Terminate(cmd)
	return cmd.Run()
}

// Detect returns the inventory to report, sorted by id.
//
// allow is what policy.yaml allows for the ONE cluster this inventory is
// reported to (apps.homes.<home>.allow). An app that is present but not
// allowed is reported with allowed=false rather than omitted: the portal
// then shows it as present-and-blocked, which is the state an operator
// can act on. Omitting it would render identically to "not installed",
// and send them looking for the wrong problem.
//
// An app that is not on PATH is omitted entirely -- there is nothing to
// report and nothing to act on.
func (d *Detector) Detect(ctx context.Context, allow []string) []Info {
	allowed := make(map[string]bool, len(allow))
	for _, a := range allow {
		allowed[strings.ToLower(strings.TrimSpace(a))] = true
	}

	home := d.home()
	var out []Info
	for _, spec := range Specs() {
		path, err := d.lookPath(spec.Binary)
		if err != nil || strings.TrimSpace(path) == "" {
			continue
		}
		state := probeAuth(spec.ID, home)
		// The descriptor is resolved for a BLOCKED app too. A descriptor
		// for a blocked app is still a descriptor: it lets the portal
		// say "present, blocked, would be driven through
		// codex-app-server" instead of rendering the row identically to
		// "not installed".
		resolved := d.resolveHarness(ctx, spec, path)
		info := Info{
			Id:               spec.ID,
			Version:          Truncate(d.version(ctx, spec, path)),
			SignedIn:         state.signedIn,
			Subscription:     NormalizeSubscription(state.subscription),
			Allowed:          allowed[spec.ID],
			Harness:          resolved.Harness,
			StructuredResult: resolved.StructuredResult,
			FollowUps:        resolved.FollowUps,
			InferenceOnly:    resolved.InferenceOnly,
		}
		if d.Readiness != nil {
			if report, ok := d.Readiness.Report(spec.ID); ok {
				info.applyReadiness(report)
			}
		}
		out = append(out, info)
	}
	// Stable order. The engine sorts by id anyway, but an unstable one
	// here would rewrite the registration row on every beat for no
	// actual change.
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// applyReadiness lays what was measured over what the files said.
//
// MEASURED BEATS THE FILE, IN BOTH DIRECTIONS. A signed-out answer clears
// SignedIn, because an expired login leaves its files in place, and doing it here
// gives an engine that predates readiness the right answer too. An app that
// answered has a login whatever the files say, which is the macOS case probeAuth
// states plainly: a token in the Keychain and no account record.
func (i *Info) applyReadiness(report ReadinessReport) {
	i.Readiness = report.State
	i.ReadinessCheckedAt = report.CheckedAt
	i.UsageResetsAt = report.ResetsAt
	i.ReadinessDetail = Truncate(report.Detail)
	switch report.State {
	case harness.ReadinessSignedOut:
		i.SignedIn = false
	case harness.ReadinessReady, harness.ReadinessOutOfUsage:
		i.SignedIn = true
	}
}

// ResolveSpec returns the spec for one app id with its harness resolved
// against the binary this machine actually has.
//
// The app-session runner needs that answer at the moment it starts a
// session, and it must be the SAME answer the registration carried --
// two implementations of "which Codex is this" could disagree, and
// nothing on the machine would say which one lied. So this shares
// Detect's cache rather than re-deriving anything.
//
// ok=false means "do not start a session": the id is outside the closed
// set, or the binary is not on PATH. The two are deliberately one answer,
// because they are one answer to the only question the caller has.
//
// The returned Binary is the PATH IT RESOLVED, not the bare name. The
// session starts exactly the binary this lookup found and the inventory
// reported, rather than a second lookup at exec time that a different PATH
// -- a LaunchAgent's is not the owner's shell's -- could answer differently.
func (d *Detector) ResolveSpec(ctx context.Context, id string) (Spec, bool) {
	spec, ok := SpecFor(strings.TrimSpace(id))
	if !ok {
		return Spec{}, false
	}
	path, err := d.lookPath(spec.Binary)
	if err != nil || strings.TrimSpace(path) == "" {
		return Spec{}, false
	}
	resolved := d.resolveHarness(ctx, spec, path)
	resolved.Binary = path
	return resolved, true
}

// Version returns the app's own version string, as Detect reports it and
// from the same cache, or "" when the app is not on PATH or would not say.
//
// The session fingerprint asks this. In the worker that is a Detector of
// the session manager's own, not the one the inventory reports from, so
// the fingerprint's version is a second probe of the same binary rather
// than the registration's own answer. Both caches are keyed on the
// binary's size and mtime, so the two differ only across an upgrade --
// and then the fingerprint, taken at the session's start, is the one
// describing the binary that ran.
func (d *Detector) Version(ctx context.Context, id string) string {
	spec, ok := SpecFor(strings.TrimSpace(id))
	if !ok {
		return ""
	}
	path, err := d.lookPath(spec.Binary)
	if err != nil || strings.TrimSpace(path) == "" {
		return ""
	}
	return Truncate(d.version(ctx, spec, path))
}

// resolveHarness returns spec with its harness fields settled for the
// binary at path.
//
// A probe that fails for ANY reason -- no such subcommand, a timeout, a
// binary that will not execute -- leaves the floor in place. The two
// directions of that error do not cost the same: driving an
// app-server-capable Codex through the mcp-server tools loses usage
// numbers and structured answers, while driving an old Codex through a
// protocol it does not have kills the session at Start, after the engine
// has already committed a turn to this machine.
func (d *Detector) resolveHarness(ctx context.Context, spec Spec, path string) Spec {
	upgraded, probe, ok := spec.harnessUpgrade()
	if !ok {
		return spec
	}
	if !d.harnessUpgraded(ctx, spec.ID, path, probe) {
		return spec
	}
	return upgraded
}

// harnessUpgraded runs the probe, or reuses its cached verdict.
func (d *Detector) harnessUpgraded(ctx context.Context, id, path string, probe []string) bool {
	stamp := binaryStamp(path)
	ttl := d.VersionTTL
	if ttl <= 0 {
		ttl = DefaultVersionTTL
	}

	d.mu.Lock()
	if entry, ok := d.harnessCache[id]; ok && entry.stamp == stamp && d.now().Sub(entry.at) < ttl {
		d.mu.Unlock()
		return entry.upgraded
	}
	d.mu.Unlock()

	upgraded := d.runProbe(ctx, path, probe) == nil

	d.mu.Lock()
	if d.harnessCache == nil {
		d.harnessCache = make(map[string]harnessEntry, len(Specs()))
	}
	d.harnessCache[id] = harnessEntry{upgraded: upgraded, stamp: stamp, at: d.now()}
	d.mu.Unlock()
	return upgraded
}

// version returns the app's own version string, cached against the
// binary's identity.
func (d *Detector) version(ctx context.Context, spec Spec, path string) string {
	stamp := binaryStamp(path)
	ttl := d.VersionTTL
	if ttl <= 0 {
		ttl = DefaultVersionTTL
	}

	d.mu.Lock()
	if entry, ok := d.cache[spec.ID]; ok && entry.stamp == stamp && d.now().Sub(entry.at) < ttl {
		d.mu.Unlock()
		return entry.version
	}
	d.mu.Unlock()

	raw, err := d.runVersion(ctx, path, spec.VersionArgs)
	if err != nil {
		// A binary that will not report its version is still present,
		// and still worth reporting: the engine reads an unparseable
		// version as "any version" rather than as absent. Do not cache
		// the failure -- a half-finished install should resolve itself
		// on the next beat, not in five minutes.
		return ""
	}
	version := strings.TrimSpace(raw)
	if i := strings.IndexAny(version, "\r\n"); i >= 0 {
		version = strings.TrimSpace(version[:i])
	}

	d.mu.Lock()
	if d.cache == nil {
		d.cache = make(map[string]versionEntry, len(Specs()))
	}
	d.cache[spec.ID] = versionEntry{version: version, stamp: stamp, at: d.now()}
	d.mu.Unlock()
	return version
}

// binaryStamp identifies a binary by size and mtime, so replacing it in
// place invalidates the cached version without waiting out the TTL.
func binaryStamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return path
	}
	return path + "|" + fi.ModTime().UTC().Format(time.RFC3339Nano) + "|" + itoa(fi.Size())
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// authState is what an app's own files say about itself.
type authState struct {
	signedIn     bool
	subscription string
}

// probeAuth reads the app's OWN state files.
//
// It never shells out to a real prompt to find out -- that spends the
// user's tokens to answer a status question, on a 15-second cadence -- and
// it never touches the OS keyring. The worker runs as a LaunchAgent on
// macOS and a keyring read there raises a Keychain prompt on a machine
// whose owner is not necessarily at it; the same reason the worker's run
// path already keeps the lazy file default for its own credentials.
//
// The consequence is stated plainly rather than papered over: on a macOS
// box where Claude Code put its token in the Keychain and wrote no
// account record, this reports signedIn=false. That is the designed
// direction of the error. The machine is simply not selected, and the
// portal says "not signed in" -- rather than showing a green row for a
// machine that will refuse the run after a plan has committed to it.
func probeAuth(id, home string) authState {
	if strings.TrimSpace(home) == "" {
		return authState{}
	}
	switch id {
	case IDClaudeCode:
		return probeClaudeCode(home)
	case IDCodex:
		return probeCodex(home)
	}
	return authState{}
}

func probeClaudeCode(home string) authState {
	state := authState{}

	// ~/.claude/.credentials.json is the token file Claude Code writes
	// where no OS keyring is available. Its presence with a non-empty
	// access token is a positive signal on its own.
	if obj, ok := readJSONObject(filepath.Join(home, ".claude", ".credentials.json")); ok {
		if oauth, ok := obj["claudeAiOauth"].(map[string]any); ok {
			if s, _ := oauth["accessToken"].(string); strings.TrimSpace(s) != "" {
				state.signedIn = true
			}
			state.subscription = reportedSubscription(oauth)
		}
	}

	// ~/.claude.json carries the account record. On macOS the token
	// itself lives in the Keychain, which this must not read, so the
	// account record is the only non-prompting signal available.
	if obj, ok := readJSONObject(filepath.Join(home, ".claude.json")); ok {
		if account, ok := obj["oauthAccount"].(map[string]any); ok {
			uuid, _ := account["accountUuid"].(string)
			email, _ := account["emailAddress"].(string)
			if strings.TrimSpace(uuid) != "" || strings.TrimSpace(email) != "" {
				state.signedIn = true
			}
			if sub := reportedSubscription(account); sub != "" {
				state.subscription = sub
			}
		}
		if sub := reportedSubscription(obj); sub != "" {
			state.subscription = sub
		}
	}
	return state
}

func probeCodex(home string) authState {
	state := authState{}
	obj, ok := readJSONObject(filepath.Join(home, ".codex", "auth.json"))
	if !ok {
		return state
	}
	if key, _ := obj["OPENAI_API_KEY"].(string); strings.TrimSpace(key) != "" {
		state.signedIn = true
	}
	if tokens, ok := obj["tokens"].(map[string]any); ok {
		access, _ := tokens["access_token"].(string)
		idTok, _ := tokens["id_token"].(string)
		if strings.TrimSpace(access) != "" || strings.TrimSpace(idTok) != "" {
			state.signedIn = true
		}
		// Codex stores its reported ChatGPT plan in the ID token's auth claim.
		// This is display metadata only, never an authentication/permission gate.
		state.subscription = codexReportedPlan(idTok)
		if sub := reportedSubscription(tokens); sub != "" {
			state.subscription = sub
		}
	}
	if sub := reportedSubscription(obj); sub != "" {
		state.subscription = sub
	}
	return state
}

// subscriptionKeys are the keys an app may write its own plan into. This
// is a READ of what the app said, not a derivation: a key that is absent
// leaves the answer at "unknown", and no other field is consulted to fill
// it in. "unknown" is the expected value on most machines today, and the
// engine records it as billing "unknown" rather than as free.
var subscriptionKeys = []string{"subscriptionType", "subscription", "chatgpt_plan_type", "planType"}

// reportedSubscription maps a plan value the app wrote to the closed set.
// Returns "" when the app said nothing, so a caller can distinguish that
// from a deliberate "unknown".
func reportedSubscription(obj map[string]any) string {
	for _, key := range subscriptionKeys {
		raw, present := obj[key]
		if !present {
			continue
		}
		value, ok := raw.(string)
		if !ok {
			continue
		}
		value = strings.ToLower(strings.TrimSpace(value))
		switch value {
		case "":
			continue
		case "none", "free":
			return SubscriptionNone
		case "unknown":
			return SubscriptionUnknown
		default:
			// A named plan -- "pro", "max", "team", "plus", "enterprise"
			// -- is the app stating it has one. The specific tier is not
			// modelled: the engine's closed set is three values, and it
			// bills subscription runs the same way whichever tier it is.
			return SubscriptionPresent
		}
	}
	return ""
}

// readJSONObject reads a JSON object, returning ok=false for anything
// missing, unreadable or not an object. A malformed state file must read
// as "cannot tell", never as an error that stops the heartbeat.
func readJSONObject(path string) (map[string]any, bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, false
	}
	return obj, true
}

// Decode only the bounded plan claim, never return or log the token payload.
// Missing, malformed and expired claims remain unknown. No network or keychain
// access is needed on the heartbeat path.
func codexReportedPlan(token string) string {
	if len(token) > 64*1024 {
		return ""
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	expiry, ok := claims["exp"].(float64)
	if !ok || expiry <= float64(time.Now().Unix()) {
		return ""
	}
	account, ok := claims["https://api.openai.com/auth"].(map[string]any)
	if !ok {
		return ""
	}
	return reportedSubscription(account)
}
