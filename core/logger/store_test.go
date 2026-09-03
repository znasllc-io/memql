package logger

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/common"
)

// captureSink records every line it is handed. The test double for
// component/logstore.Sink.
type captureSink struct {
	mu    sync.Mutex
	lines []Line
}

func (c *captureSink) Write(l Line) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, l)
}

func (c *captureSink) all() []Line {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Line(nil), c.lines...)
}

// storeLogger builds the production chain -- redactor around fanout(json,
// store) -- at an explicit store floor, writing its console output to buf.
func storeLogger(buf *bytes.Buffer, consoleLevel, storeFloor slog.Level, off bool) *slog.Logger {
	json := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: consoleLevel})
	return slog.New(NewRedactingHandler(newFanoutHandler(json, newStoreHandlerAt(storeFloor, off))))
}

func TestPreBootRingKeepsLinesAndDrainsThemInOrder(t *testing.T) {
	resetStoreForTest()
	t.Cleanup(resetStoreForTest)

	var buf bytes.Buffer
	log := storeLogger(&buf, slog.LevelInfo, slog.LevelInfo, false)
	for i := 0; i < 5; i++ {
		log.Info(fmt.Sprintf("boot line %d", i), "component", "boot")
	}
	if got := pendingRingLen(); got != 5 {
		t.Fatalf("ring holds %d lines before a sink is registered, want 5", got)
	}

	sink := &captureSink{}
	SetSink(sink)
	if got := pendingRingLen(); got != 0 {
		t.Errorf("ring still holds %d lines after SetSink; the drain must empty it", got)
	}
	lines := sink.all()
	if len(lines) != 5 {
		t.Fatalf("sink received %d lines from the drain, want 5", len(lines))
	}
	for i, l := range lines {
		if want := fmt.Sprintf("boot line %d", i); l.Message != want {
			t.Errorf("drained line %d is %q, want %q -- the ring must drain in write order", i, l.Message, want)
		}
	}

	// After the drain, lines go straight to the sink.
	log.Info("after boot", "component", "boot")
	if got := sink.all(); len(got) != 6 || got[5].Message != "after boot" {
		t.Errorf("a line written after SetSink did not reach the sink directly: %d lines", len(got))
	}
}

func TestPreBootRingDropsTheOldestWhenFull(t *testing.T) {
	resetStoreForTest()
	t.Cleanup(resetStoreForTest)

	var buf bytes.Buffer
	log := storeLogger(&buf, slog.LevelError, slog.LevelInfo, false)
	total := preBootRingSize + 10
	for i := 0; i < total; i++ {
		log.Info(fmt.Sprintf("line %d", i))
	}
	sink := &captureSink{}
	SetSink(sink)
	lines := sink.all()
	if len(lines) != preBootRingSize {
		t.Fatalf("drained %d lines, want the ring's %d", len(lines), preBootRingSize)
	}
	if lines[0].Message != "line 10" {
		t.Errorf("oldest drained line is %q, want %q -- the ring drops the OLDEST when full", lines[0].Message, "line 10")
	}
	if last := lines[len(lines)-1].Message; last != fmt.Sprintf("line %d", total-1) {
		t.Errorf("newest drained line is %q, want the last written", last)
	}
}

func TestStoreFloorAndOff(t *testing.T) {
	resetStoreForTest()
	t.Cleanup(resetStoreForTest)
	sink := &captureSink{}
	SetSink(sink)

	var buf bytes.Buffer
	// Console at info, store at warn: an info line prints and is not stored.
	log := storeLogger(&buf, slog.LevelInfo, slog.LevelWarn, false)
	log.Info("printed only")
	log.Warn("printed and stored")
	if !strings.Contains(buf.String(), "printed only") {
		t.Errorf("the console lost an info line; the store floor must not touch the console level")
	}
	lines := sink.all()
	if len(lines) != 1 || lines[0].Message != "printed and stored" {
		t.Fatalf("store received %v, want exactly the warn line", messages(lines))
	}

	// Console at info, store at debug: a debug line is stored and NOT printed.
	buf.Reset()
	log = storeLogger(&buf, slog.LevelInfo, slog.LevelDebug, false)
	log.Debug("stored only")
	if strings.Contains(buf.String(), "stored only") {
		t.Errorf("a debug line reached the console at level info; fanout Enabled must be the OR, not a widening of the console")
	}
	if lines := sink.all(); len(lines) != 2 || lines[1].Message != "stored only" {
		t.Errorf("a debug line below the console level but at the store floor was not stored: %v", messages(lines))
	}

	// off disables the store on this node and changes nothing on the console.
	buf.Reset()
	log = storeLogger(&buf, slog.LevelInfo, slog.LevelInfo, true)
	log.Error("console still works")
	if !strings.Contains(buf.String(), "console still works") {
		t.Errorf("off silenced the console")
	}
	if lines := sink.all(); len(lines) != 2 {
		t.Errorf("off stored a line: %v", messages(lines))
	}
}

func TestParseStoreLevel(t *testing.T) {
	cases := []struct {
		in   string
		name string
		off  bool
	}{
		{"", "info", false},
		{"info", "info", false},
		{"DEBUG", "debug", false},
		{" warn ", "warn", false},
		{"error", "error", false},
		{"off", "off", true},
		{"nonsense", "info", false},
	}
	for _, tc := range cases {
		_, off, name := parseStoreLevel(tc.in)
		if name != tc.name || off != tc.off {
			t.Errorf("parseStoreLevel(%q) = (%s, off=%v), want (%s, off=%v)", tc.in, name, off, tc.name, tc.off)
		}
	}
}

func TestRecursionGuardKeepsTheStoresOwnLinesOut(t *testing.T) {
	resetStoreForTest()
	t.Cleanup(resetStoreForTest)
	sink := &captureSink{}
	SetSink(sink)

	var buf bytes.Buffer
	log := storeLogger(&buf, slog.LevelInfo, slog.LevelInfo, false)
	log.Warn("insert failed", "component", "logs.store")
	log.Warn("flush failed", "component", "logs.store.flush")
	// The derived-logger form, which is how the sink itself is built.
	log.With("component", "logs.store").Error("database gone")
	// And the gap line the sink logs under `logs` -- NOT the guarded prefix,
	// so the store shows its own gaps.
	log.Warn("logs: dropped 3 lines", "component", "logs")

	if !strings.Contains(buf.String(), "insert failed") || !strings.Contains(buf.String(), "database gone") {
		t.Errorf("the guard must keep the store's lines on the console, where a broken store can be read")
	}
	lines := sink.all()
	if len(lines) != 1 || lines[0].Component != "logs" {
		t.Fatalf("store received %v; only the `logs` gap line may be stored", messages(lines))
	}
}

func TestTokenAttributeArrivesRedacted(t *testing.T) {
	resetStoreForTest()
	t.Cleanup(resetStoreForTest)
	sink := &captureSink{}
	SetSink(sink)

	var buf bytes.Buffer
	log := storeLogger(&buf, slog.LevelInfo, slog.LevelInfo, false)
	log.Info("sign-in", "token", "mql_pat_secret-value", "user", "u1")
	// The derived-logger form: WithAttrs runs through the redactor too.
	log.With("api_key", "sk-live").Info("derived")
	// A group carrying a secret: redaction recurses.
	log.Info("grouped", slog.Group("session", "cookie", "abc", "id", "s1"))

	lines := sink.all()
	if len(lines) != 3 {
		t.Fatalf("store received %d lines, want 3", len(lines))
	}
	if got := lines[0].Attributes["token"]; got != RedactedPlaceholder {
		t.Errorf("token stored as %v, want %q -- the store handler must sit INSIDE the redactor", got, RedactedPlaceholder)
	}
	if got := lines[0].Attributes["user"]; got != "u1" {
		t.Errorf("a non-secret attribute was altered: %v", got)
	}
	if got := lines[1].Attributes["api_key"]; got != RedactedPlaceholder {
		t.Errorf("a WithAttrs-bound api_key stored as %v, want %q", got, RedactedPlaceholder)
	}
	if got := lines[2].Attributes["session.cookie"]; got != RedactedPlaceholder {
		t.Errorf("a cookie inside a group stored as %v, want %q", got, RedactedPlaceholder)
	}
	if got := lines[2].Attributes["session.id"]; got != "s1" {
		t.Errorf("session.id stored as %v, want s1", got)
	}
	for _, raw := range []string{"mql_pat_secret-value", "sk-live", "abc"} {
		for _, l := range lines {
			for k, v := range l.Attributes {
				if s, ok := v.(string); ok && s == raw {
					t.Errorf("raw secret %q survived into attribute %q", raw, k)
				}
			}
		}
	}
}

func TestSubjectLandsInBothFields(t *testing.T) {
	resetStoreForTest()
	t.Cleanup(resetStoreForTest)
	sink := &captureSink{}
	SetSink(sink)

	var buf bytes.Buffer
	log := storeLogger(&buf, slog.LevelInfo, slog.LevelInfo, false)
	log.Info("deploy started", "component", "packages.pipeline",
		Subject("v1:platform:packageDeployment", "dep-1"), "package", "pkg-1")

	lines := sink.all()
	if len(lines) != 1 {
		t.Fatalf("store received %d lines, want 1", len(lines))
	}
	l := lines[0]
	if l.Subject != "dep-1" || l.SubjectConcept != "v1:platform:packageDeployment" {
		t.Errorf("Subject landed as (%q, %q), want (dep-1, v1:platform:packageDeployment)", l.Subject, l.SubjectConcept)
	}
	if l.Component != "packages.pipeline" {
		t.Errorf("component landed as %q", l.Component)
	}
	for _, lifted := range []string{"subject.id", "subject.concept", "component", "subject"} {
		if _, still := l.Attributes[lifted]; still {
			t.Errorf("%q is still in Attributes after being lifted into its own field", lifted)
		}
	}
	if l.Attributes["package"] != "pkg-1" {
		t.Errorf("an ordinary attribute beside the subject was lost: %v", l.Attributes)
	}
	// The wire form of the seam: a Group named subject with id + concept.
	if a := Subject("c", "i"); a.Key != "subject" || a.Value.Kind() != slog.KindGroup {
		t.Errorf("Subject must be an slog.Group named subject; got key=%q kind=%v", a.Key, a.Value.Kind())
	}
}

func TestGroupsFlattenToDottedKeys(t *testing.T) {
	resetStoreForTest()
	t.Cleanup(resetStoreForTest)
	sink := &captureSink{}
	SetSink(sink)

	var buf bytes.Buffer
	base := storeLogger(&buf, slog.LevelInfo, slog.LevelInfo, false)
	at := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	base.Info("record group",
		slog.Group("request", "method", "GET", slog.Group("peer", "ip", "203.0.113.9")),
		"took", 1500*time.Millisecond,
		"at", at,
		"err", errors.New("boom"),
		"count", 3,
		"ratio", 0.5,
		"ok", true,
		"shape", struct{ A int }{A: 1})
	// WithGroup on a derived logger prefixes the attrs that follow, and a
	// WithAttrs bound BEFORE the group stays un-prefixed.
	base.With("component", "svc").WithGroup("http").With("route", "/x").Info("derived group", "status", 200)

	lines := sink.all()
	if len(lines) != 2 {
		t.Fatalf("store received %d lines, want 2", len(lines))
	}
	got := lines[0].Attributes
	want := map[string]any{
		"request.method":  "GET",
		"request.peer.ip": "203.0.113.9",
		"took":            "1.5s",
		"at":              "2026-09-03T12:00:00Z",
		"err":             "boom",
		"count":           int64(3),
		"ratio":           0.5,
		"ok":              true,
		"shape":           "{1}",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("attribute %q = %#v, want %#v", k, got[k], v)
		}
	}
	d := lines[1]
	if d.Component != "svc" {
		t.Errorf("a component bound before WithGroup landed as %q, want svc", d.Component)
	}
	if d.Attributes["http.route"] != "/x" || d.Attributes["http.status"] != int64(200) {
		t.Errorf("WithGroup did not prefix the attrs that followed it: %v", d.Attributes)
	}
}

func TestDefaultLoggerReachesTheSinkAndWithAttrsCarriesComponent(t *testing.T) {
	resetStoreForTest()
	t.Cleanup(resetStoreForTest)
	sink := &captureSink{}
	SetSink(sink)

	// The production constructor, exactly as app/ builds it, then installed
	// as the process default the way newApp does -- so a slog.Default()
	// fallback anywhere in the tree reaches the store.
	var buf bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(New(common.ComponentName("unit-test"), &buf, slog.LevelInfo))

	slog.Info("via slog.Default", "k", "v")
	slog.Default().With("component", "worker.router").Warn("derived component")

	lines := sink.all()
	if len(lines) < 2 {
		t.Fatalf("store received %d lines, want at least 2 (the process floor may be off in this environment: MEMQL_LOGS_LEVEL=%q)", len(lines), StoreLevelName())
	}
	first := lines[len(lines)-2]
	if first.Message != "via slog.Default" || first.Component != "unit-test" {
		t.Errorf("slog.Default() line landed as message=%q component=%q; logger.New's With(component) must reach the store", first.Message, first.Component)
	}
	if first.Attributes["k"] != "v" {
		t.Errorf("attribute lost on the default-logger path: %v", first.Attributes)
	}
	second := lines[len(lines)-1]
	if second.Component != "worker.router" {
		t.Errorf("a WithAttrs component on a derived logger landed as %q, want worker.router (the derived value must override the constructor's)", second.Component)
	}
	if !strings.Contains(buf.String(), "via slog.Default") {
		t.Errorf("the console lost the line; the fanout must keep printing")
	}
}

func TestStoreHandlerNeverForwardsWhenNoSinkAndNothingElseChanges(t *testing.T) {
	resetStoreForTest()
	t.Cleanup(resetStoreForTest)
	var buf bytes.Buffer
	log := storeLogger(&buf, slog.LevelInfo, slog.LevelInfo, false)
	log.Info("no sink yet")
	if !strings.Contains(buf.String(), "no sink yet") {
		t.Fatalf("console output missing with no sink registered")
	}
	if CurrentSink() != nil {
		t.Fatalf("CurrentSink is not nil before SetSink")
	}
}

func messages(lines []Line) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.Message)
	}
	return out
}
