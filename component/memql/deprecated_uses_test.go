package memql

// deprecated_uses_test.go -- a deprecated language form loads with a warning
// naming its replacement, is counted, and is refused only once its window is
// spent (memql#5390).
//
// The three claims are tested against ONE table and TWO releases. Nothing about
// a form changes between the load that warns and the load that refuses: the
// test names 0.25.0 and sees what a 0.25.0 cluster will see. That is what makes
// "it refuses after the window" checkable today rather than in two releases.

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/deprecation"
	"github.com/znasllc-io/memql/component/memql/baseloader"
	"github.com/znasllc-io/memql/component/metrics"
	"github.com/znasllc-io/memql/core/component"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// deprecatedTicket is one concept spelling one field in the deprecated form.
const deprecatedTicket = "/// A ticket.\nconcept ticket {\n  /// Tags.\n  tags array(string)\n}\n"

// bootOverlayEngineForTest mounts every domain of tree over the embedded tree
// and runs Init with no database, the construction LintUnifiedTree uses, and
// returns the engine with the Init error. unmount restores the embedded-only
// tree and registry; it runs at cleanup too, and a second call is a no-op.
func bootOverlayEngineForTest(t *testing.T, tree fs.FS) (eng *MemQLEngine, initErr error, unmount func()) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	_, _, release := memqldsl.MountOverlayDomains(logger, tree)
	done := false
	unmount = func() {
		if done {
			return
		}
		done = true
		release()
		concept.ReplaceAll(nil)
		_, _ = LoadUnifiedConcepts(logger)
	}
	t.Cleanup(unmount)
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := New(nil, (&component.Component{}).WithLoggerWriter(io.Discard))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	eng.Logger = logger
	return eng, eng.Init(concept.DefaultRegistry()), unmount
}

// The whole mechanism end to end, on one tree: inside the window the form
// LOADS, WARNS naming its replacement and the release, and is COUNTED; past the
// window the same tree is REFUSED naming the same replacement.
func TestADeprecatedFormWarnsCountsAndRefusesOnlyAfterTheWindow(t *testing.T) {
	restore := deprecation.SetCurrent("0.23.0")
	tree := fstest.MapFS{
		"deptest/memql.toml":     {Data: []byte("memql = \"1.0\"\nedition = \"2026\"\n")},
		"deptest/concepts.memql": {Data: []byte(deprecatedTicket)},
	}
	before := metrics.DSLDeprecatedUsesValue(deprecation.ArrayType)
	eng, initErr, unmount := bootOverlayEngineForTest(t, tree)
	if initErr != nil {
		t.Fatalf("a tree whose only finding is a deprecated form must boot: %v", initErr)
	}
	uses := eng.DeprecatedUses()
	if len(uses) != 1 || uses[0].Rule != deprecation.ArrayType || uses[0].Line != 4 {
		t.Fatalf("uses = %+v", uses)
	}
	if uses[0].File != "deptest/concepts.memql" || uses[0].Column != 8 || uses[0].Text != "array(string)" {
		t.Errorf("the use must name its file, column and text as written: %+v", uses[0])
	}
	warnings := eng.LoadReport().WarningsSnapshot()
	if len(warnings) != 1 || !strings.Contains(warnings[0].Message, "`[]T`") || !strings.Contains(warnings[0].Message, "0.25") {
		t.Fatalf("warnings = %+v", warnings)
	}
	f, _ := deprecation.Lookup(deprecation.ArrayType)
	if w := warnings[0]; w.Code != f.Rule || w.Message != f.Warning() || w.File != "deptest/concepts.memql" || w.Line != 4 || w.Column != 8 {
		t.Errorf("the warning must carry the form's rule and exact message at the use: %+v", w)
	}
	if eng.LoadReport().HasProblems() {
		t.Fatal("a deprecated form must load: strict boot would refuse this tree")
	}
	if got := metrics.DSLDeprecatedUsesValue(deprecation.ArrayType) - before; got != 1 {
		t.Fatalf("counter moved by %v, want 1", got)
	}
	unmount()
	restore()

	// Same table, same tree, a release past the window.
	restore = deprecation.SetCurrent("0.25.0")
	defer restore()
	diags, _, _ := LintUnifiedTree(nil, tree)
	found := false
	for _, d := range diags {
		if d.Code == deprecation.ArrayType && d.Severity == LintSeverityError && strings.Contains(d.Message, "`[]T`") {
			found = true
		}
	}
	if !found {
		t.Fatalf("after the window the form must be refused with its replacement named; diags = %+v", diags)
	}
}

// Past its window a form refuses the strict boot the way a retired form does: a
// coded skip whose text is the form's refusal, and no warning beside it.
func TestAnExpiredDeprecatedFormRefusesStrictBoot(t *testing.T) {
	t.Setenv(AllowSkipsEnvVar, "")
	restore := deprecation.SetCurrent("0.25.0")
	defer restore()
	f, _ := deprecation.Lookup(deprecation.ArrayType)

	eng, initErr, _ := bootOverlayEngineForTest(t, fstest.MapFS{
		"deprefused/memql.toml":     {Data: []byte("memql = \"1.0\"\nedition = \"2026\"\n")},
		"deprefused/concepts.memql": {Data: []byte(deprecatedTicket)},
	})
	if initErr == nil || !strings.Contains(initErr.Error(), "strict DSL boot refused") || !strings.Contains(initErr.Error(), f.Refusal()) {
		t.Fatalf("Init must refuse the tree naming the refusal, got %v", initErr)
	}
	var coded bool
	for _, s := range eng.LoadReport().Skipped {
		if s.Code == deprecation.ArrayType && s.File == "deprefused/concepts.memql" && strings.Contains(s.Err, "line 4, column 8") {
			coded = true
		}
	}
	if !coded {
		t.Errorf("the refusal must be a coded skip positioned on the use: %+v", eng.LoadReport().Skipped)
	}
	if w := eng.LoadReport().WarningsSnapshot(); len(w) != 0 {
		t.Errorf("a form past its window is a problem, not a warning: %+v", w)
	}
	if uses := eng.DeprecatedUses(); len(uses) != 1 {
		t.Errorf("a refused form's use is still a use the tree makes: %+v", uses)
	}
}

// Every use is its own warning, and DeprecatedUses lists them in file and line
// order whatever order the tree walks in.
func TestDeprecatedUsesAreOnePerUseInFileAndLineOrder(t *testing.T) {
	restore := deprecation.SetCurrent("0.23.0")
	defer restore()
	const concepts = "/// A widget.\nconcept widget {\n  /// Names.\n  names array(string)\n" +
		"  /// Counts.\n  counts []array(int)\n}\n"
	eng, initErr, unmount := bootOverlayEngineForTest(t, fstest.MapFS{
		"depmany/memql.toml":     {Data: []byte("memql = \"1.0\"\nedition = \"2026\"\n")},
		"depmany/concepts.memql": {Data: []byte(concepts)},
	})
	if initErr != nil {
		t.Fatalf("Init: %v", initErr)
	}
	uses := eng.DeprecatedUses()
	if len(uses) != 2 || uses[0].Line != 4 || uses[1].Line != 6 {
		t.Fatalf("uses = %+v", uses)
	}
	if got := len(eng.LoadReport().WarningsSnapshot()); got != 2 {
		t.Errorf("want one warning per use, got %d", got)
	}
	unmount()

	diags := lint(t, fstest.MapFS{"depmany/concepts.memql": {Data: []byte(concepts)}})
	var warnings []LintDiagnostic
	for _, d := range diags {
		if d.Severity == LintSeverityWarning {
			warnings = append(warnings, d)
		}
	}
	if len(warnings) != 2 || warnings[0].Line != 4 || warnings[1].Line != 6 || warnings[0].Column != 9 {
		t.Fatalf("the parity pass must carry each use as its own positioned warning, got %+v", warnings)
	}
}

// Every diagnostic the parity pass made before warnings existed is an error. An
// empty Severity would read as a warning to a caller that branches on
// IsWarning, which is how a refusal stops failing a deploy.
func TestLintDiagnosticsWithoutASeverityAreErrors(t *testing.T) {
	diags := lint(t, fstest.MapFS{
		"lintsev/concepts.memql": {Data: []byte("@version(\"1.0.0\")\n@description(\"A widget.\")\nconcept widget {\n  enabled  boolean  @description(\"On.\")\n}\n")},
	})
	if len(diags) == 0 {
		t.Fatal("the fixture must produce a diagnostic")
	}
	for _, d := range diags {
		if d.Severity != LintSeverityError {
			t.Errorf("diagnostic %+v has severity %q, want %q", d, d.Severity, LintSeverityError)
		}
	}
}

// A load creates every registered form's series, and a load that reads no use
// of a form does not count one. Without the first an alert over a form nobody
// uses evaluates to no data, which reads exactly like the form being used and
// the counter never scraped.
func TestALoadCreatesEveryDeprecatedFormsMetricSeries(t *testing.T) {
	before := map[string]float64{}
	for _, f := range deprecation.Forms() {
		before[f.Rule] = metrics.DSLDeprecatedUsesValue(f.Rule)
	}
	recordDeprecatedUses(newLoadReport(),
		[]baseloader.RawFile{{Path: "probe/concepts.memql", Content: "concept ticket {\n  note string\n}\n"}},
		nil, "0.23.0")

	families, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	series := map[string]float64{}
	for _, fam := range families {
		if fam.GetName() != "memql_dsl_deprecated_uses_total" {
			continue
		}
		for _, m := range fam.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "rule" {
					series[l.GetValue()] = m.GetCounter().GetValue()
				}
			}
		}
	}
	for _, f := range deprecation.Forms() {
		got, ok := series[f.Rule]
		if !ok {
			t.Errorf("memql_dsl_deprecated_uses_total has no series for rule %q after a load", f.Rule)
			continue
		}
		if got != before[f.Rule] {
			t.Errorf("rule %q counted %v uses in a load that read none", f.Rule, got-before[f.Rule])
		}
	}
}

// The boot summary says how many warnings the load carried, on the healthy line
// too: a tree that loads clean may still use a form a later release refuses.
func TestLoadReportSummaryCountsWarnings(t *testing.T) {
	r := newLoadReport()
	r.AddWarning(deprecatedWarningForTest())
	var buf bytes.Buffer
	r.logSummary(slog.New(slog.NewJSONHandler(&buf, nil)))
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("summary is not one JSON line: %v\n%s", err, buf.String())
	}
	if line["warnings"] != float64(1) {
		t.Fatalf("the summary must count the warnings, got %v", line)
	}
}

// Each use is its own WARN line naming where it is, so an operator reading a
// boot log can go straight to it; a file that spells nothing logs nothing.
func TestDeprecatedUsesLogOneWarnLinePerUse(t *testing.T) {
	var buf bytes.Buffer
	report := newLoadReport()
	recordDeprecatedUses(report, []baseloader.RawFile{
		{Path: "probe/concepts.memql", Content: "concept ticket {\n  tags array(string)\n  ids  array(int)\n}\n"},
		{Path: "probe/queries.memql", Content: "query ticket open {\n  filter row => row.status == \"open\"\n}\n"},
	}, slog.New(slog.NewJSONHandler(&buf, nil)), "0.23.0")

	var at []string
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("not a JSON log line: %v\n%s", err, raw)
		}
		if line["level"] != "WARN" || line["rule"] != deprecation.ArrayType {
			t.Errorf("want a WARN line naming the rule, got %v", line)
		}
		at = append(at, line["at"].(string))
	}
	if strings.Join(at, " ") != "probe/concepts.memql:2:8 probe/concepts.memql:3:8" {
		t.Fatalf("WARN lines at %v, want one per use in order", at)
	}
}

// The release the engine decides at is the release it was CUT FROM, and an
// unstamped build -- every test binary and every `make dev` image -- names
// none. So the default is fail-open, which is what keeps a dev cluster loading
// a form its own release would refuse rather than refusing one its release
// would load.
func TestTheEngineDecidesAtTheReleaseItWasBuiltFrom(t *testing.T) {
	if got := deprecation.Current(); got != "" {
		t.Fatalf("an unstamped test binary decides at %q, want \"\" (core/buildinfo.Release)", got)
	}
	for _, f := range deprecation.Forms() {
		if f.RefusesAt(deprecation.Current()) {
			t.Errorf("form %s refuses in a build that names no release", f.Rule)
		}
	}
}

func deprecatedWarningForTest() baseloader.Warning {
	f, _ := deprecation.Lookup(deprecation.ArrayType)
	return baseloader.Warning{Component: deprecatedFormsComponent, File: "probe/concepts.memql", Line: 4, Column: 8, Code: f.Rule, Message: f.Warning()}
}
