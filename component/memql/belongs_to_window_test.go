package memql

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/deprecation"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/metrics"
)

// belongs_to_window_test.go -- a concept with no @belongsTo in the deprecation
// window (design record docs/superpowers/specs/2026-11-12-account-export-design.md,
// E5 and section C). Inside the window an undeclared mounted concept loads,
// draws the one log line it always did, is counted under the rule, and puts
// NOTHING on the load report (row-ownership R3). A release stamped past the
// window refuses one per concept; an unstamped build never refuses.
//
// These tests move deprecation.Current() and so are never parallel.

const windowSrc = "@belongsTo(cluster)\nconcept declared {\n  title  string\n}\n\n" +
	"concept zebra {\n  title  string\n}\n\n" +
	"concept apple {\n  title  string\n}\n"

func analyzeWindowTree(t *testing.T, release string, logger *slog.Logger) PackageDSLResult {
	t.Helper()
	restore := deprecation.SetCurrent(release)
	defer restore()
	result, err := AnalyzePackageDSL(logger, withLanguageLines(fstest.MapFS{
		"belongstowindow/concepts.memql": &fstest.MapFile{Data: []byte(windowSrc)},
	}))
	if err != nil {
		t.Fatalf("AnalyzePackageDSL: %v", err)
	}
	return result
}

func undeclaredDiagnostics(result PackageDSLResult) []string {
	var out []string
	for _, d := range result.Diagnostics {
		if strings.Contains(d.Message, "["+langparser.CodeBelongsToUndeclared+"]") {
			out = append(out, d.Message)
		}
	}
	return out
}

// Inside the window: no skip, no load-report warning, the existing single log
// line, and nothing else on the report.
func TestBelongsToUndeclaredInsideTheWindowOnlyWarns(t *testing.T) {
	for _, release := range []string{"0.32.0", "0.33.9"} {
		t.Run(release, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			result := analyzeWindowTree(t, release, logger)
			if len(result.Diagnostics) != 0 {
				t.Fatalf("a load inside the window drew diagnostics: %+v", result.Diagnostics)
			}
			var lines int
			for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				var line map[string]any
				if json.Unmarshal([]byte(raw), &line) != nil || line["component"] != "memql.belongsTo" {
					continue
				}
				lines++
				concepts, _ := line["concepts"].(string)
				if concepts != "v1:belongstowindow:apple, v1:belongstowindow:zebra" {
					t.Errorf("the warning names %q, want both undeclared concepts sorted", concepts)
				}
			}
			if lines != 1 {
				t.Fatalf("want one memql.belongsTo log line, got %d:\n%s", lines, buf.String())
			}
		})
	}
}

// The count is published under the rule on memql_dsl_deprecated_uses_total, one
// per undeclared concept, by a publishing load -- and a load that puts nothing
// on the report.
func TestBelongsToUndeclaredIsCountedUnderTheRule(t *testing.T) {
	concepts := []*memorynodes.Concept{
		{Name: "v1:belongstowindow:zebra"},
		{Name: "v1:belongstowindow:apple"},
		{Name: "v1:belongstowindow:declared", BelongsTo: &langparser.BelongsToDecl{Scope: langparser.BelongsToCluster}},
	}
	before := metrics.DSLDeprecatedUsesValue(deprecation.ConceptBelongsToUndeclared)
	report := newLoadReport()
	recordUndeclaredBelongsTo(report, nil, concepts, "0.32.0", true)
	if got := metrics.DSLDeprecatedUsesValue(deprecation.ConceptBelongsToUndeclared) - before; got != 2 {
		t.Fatalf("counted %v under %s, want 2", got, deprecation.ConceptBelongsToUndeclared)
	}
	if len(report.Skipped) != 0 || len(report.Warnings) != 0 {
		t.Fatalf("inside the window the report carries skips=%v warnings=%v, want none", report.Skipped, report.Warnings)
	}
	// A candidate validation publishes nothing.
	before = metrics.DSLDeprecatedUsesValue(deprecation.ConceptBelongsToUndeclared)
	recordUndeclaredBelongsTo(newLoadReport(), nil, concepts, "0.32.0", false)
	if got := metrics.DSLDeprecatedUsesValue(deprecation.ConceptBelongsToUndeclared) - before; got != 0 {
		t.Fatalf("a load that does not publish counted %v", got)
	}
}

// A release stamped 0.34.0 refuses: one coded skip per concept, naming it and
// the forms to write.
func TestBelongsToUndeclaredRefusesOnceTheWindowCloses(t *testing.T) {
	got := undeclaredDiagnostics(analyzeWindowTree(t, "0.34.0", discardLogger()))
	if len(got) != 2 {
		t.Fatalf("want one %s refusal per undeclared concept, got %d:\n%v", langparser.CodeBelongsToUndeclared, len(got), got)
	}
	for i, name := range []string{"v1:belongstowindow:apple", "v1:belongstowindow:zebra"} {
		for _, want := range []string{`"` + name + `"`, "stopped loading in 0.34", "@belongsTo(account)", "@belongsTo(cluster)", "memqllint"} {
			if !strings.Contains(got[i], want) {
				t.Errorf("refusal %d does not carry %q:\n%s", i, want, got[i])
			}
		}
	}
}

// An unstamped build -- every test binary, every dev image -- never refuses.
func TestBelongsToUndeclaredNeverRefusesInAnUnstampedBuild(t *testing.T) {
	if got := undeclaredDiagnostics(analyzeWindowTree(t, "", discardLogger())); len(got) != 0 {
		t.Fatalf("an unstamped build refused: %v", got)
	}
}

// UndeclaredAccountConcepts is what an export refuses on and what the account
// page shows: every registered concept that declares nothing, sorted. It leaves
// out only an entry with no name. The engine's own domains are NOT left out --
// unlike the load's warning -- because an authored concept promoted into a core
// namespace is in the registry as surely as a bundle's, and an export that did
// not see it would leave its rows out of the file without a word.
func TestUndeclaredAccountConceptsIsEveryUndeclaredConcept(t *testing.T) {
	cluster := &langparser.BelongsToDecl{Scope: langparser.BelongsToCluster}
	got := UndeclaredAccountConcepts([]*memorynodes.Concept{
		{Name: "v1:acmebundle:zebra"},
		nil,
		{Name: "  "},
		{Name: "v1:library:ghost"}, // a core domain's namespace
		{Name: "v1:acmebundle:apple"},
		{Name: "v1:acmebundle:declared", BelongsTo: cluster},
	})
	want := []string{"v1:acmebundle:apple", "v1:acmebundle:zebra", "v1:library:ghost"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UndeclaredAccountConcepts = %v, want %v", got, want)
	}
	if got := UndeclaredAccountConcepts(nil); len(got) != 0 {
		t.Fatalf("an empty registry answered %v", got)
	}
	// The engine's own tree, as a node loads it, holds none.
	registry := loadedConceptTree(t)
	if got := UndeclaredAccountConcepts(registry.List()); len(got) != 0 {
		t.Fatalf("the loaded engine tree holds undeclared concepts: %v", got)
	}
}
