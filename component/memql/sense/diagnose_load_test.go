package sense

import (
	"reflect"
	"testing"
)

// diagnose_load_test.go -- the load pass as Sense exposes it (memql#5434):
// DiagnoseLoad asks the engine only when it can place the document, and
// MergeLoadDiagnostics keeps one squiggle per fault.

func rng(l1, c1, l2, c2 int) Range {
	return Range{Start: Position{Line: l1, Column: c1}, End: Position{Line: l2, Column: c2}}
}

func TestMergeLoadDiagnostics_NoLoadLeavesDiagnoseAlone(t *testing.T) {
	fast := []Diagnostic{
		{Range: rng(3, 1, 3, 4), Severity: SeverityWarning, Code: "b"},
		{Range: rng(1, 1, 1, 4), Severity: SeverityError, Code: "a"},
	}
	if got := MergeLoadDiagnostics(fast, nil); !reflect.DeepEqual(got, fast) {
		t.Errorf("with no load diagnostics Diagnose's set is returned as it was, order included: %+v", got)
	}
}

func TestMergeLoadDiagnostics_DistinctFaultsAreAllKept(t *testing.T) {
	fast := []Diagnostic{{Range: rng(5, 3, 5, 9), Severity: SeverityError, Code: "parse-error"}}
	load := []Diagnostic{{Range: rng(2, 17, 2, 26), Severity: SeverityError, Code: "lower_unknown_field"}}
	got := MergeLoadDiagnostics(fast, load)
	if len(got) != 2 || got[0].Code != "lower_unknown_field" || got[1].Code != "parse-error" {
		t.Errorf("two faults on different nodes are two squiggles, in position order: %+v", got)
	}
}

// `actor.displayName`: Diagnose's closed-envelope rule and Lower both refuse
// it, both as errors. One fault, one squiggle -- Diagnose's, already drawn.
func TestMergeLoadDiagnostics_SameFaultSameSeverityKeepsDiagnose(t *testing.T) {
	fast := []Diagnostic{{Range: rng(4, 43, 4, 54), Severity: SeverityError, Code: "actor-unknown-property"}}
	load := []Diagnostic{{Range: rng(4, 37, 4, 54), Severity: SeverityError, Code: "lower_unknown_field"}}
	got := MergeLoadDiagnostics(fast, load)
	if len(got) != 1 || got[0].Code != "actor-unknown-property" {
		t.Errorf("want only Diagnose's error, got %+v", got)
	}
}

// A bare `id` in a filter: Diagnose warns, the load refuses. The load's error
// is the truth about the file, so it replaces the warning.
func TestMergeLoadDiagnostics_MoreSevereLoadReplacesTheWarning(t *testing.T) {
	fast := []Diagnostic{
		{Range: rng(4, 21, 4, 23), Severity: SeverityWarning, Code: "bare-row-intrinsic"},
		{Range: rng(9, 1, 9, 30), Severity: SeverityHint, Code: "description-length"},
	}
	load := []Diagnostic{{Range: rng(4, 21, 4, 23), Severity: SeverityError, Code: "lower_unknown_name"}}
	got := MergeLoadDiagnostics(fast, load)
	if len(got) != 2 || got[0].Code != "lower_unknown_name" || got[1].Code != "description-length" {
		t.Errorf("want the load's error in place of the warning, and the unrelated hint kept: %+v", got)
	}
}

func TestMergeLoadDiagnostics_Overlap(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b Range
		want bool
	}{
		{"touching ranges share no character", rng(1, 1, 1, 5), rng(1, 5, 1, 9), false},
		{"one character in common", rng(1, 1, 1, 6), rng(1, 5, 1, 9), true},
		{"containment", rng(1, 1, 1, 20), rng(1, 5, 1, 9), true},
		{"a caret on the first character", rng(1, 5, 1, 5), rng(1, 5, 1, 9), true},
		{"a caret just past the end", rng(1, 9, 1, 9), rng(1, 5, 1, 9), false},
		{"different lines", rng(1, 1, 1, 9), rng(2, 1, 2, 9), false},
		{"a multi-line range covering a line", rng(1, 5, 3, 2), rng(2, 1, 2, 4), true},
	} {
		if got := rangesOverlap(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: rangesOverlap = %v, want %v", tc.name, got, tc.want)
		}
		if got := rangesOverlap(tc.b, tc.a); got != tc.want {
			t.Errorf("%s (swapped): rangesOverlap = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// fakeLoadProvider is a registry provider that can run the load, recording
// what it was asked.
type fakeLoadProvider struct {
	RegistryProvider
	calls []string
	out   []Diagnostic
}

func (f *fakeLoadProvider) LoadDiagnostics(source, filePath string) []Diagnostic {
	f.calls = append(f.calls, filePath)
	return f.out
}

func TestDiagnoseLoad(t *testing.T) {
	want := []Diagnostic{{Range: rng(1, 1, 1, 2), Severity: SeverityError, Code: "lower_refused"}}
	p := &fakeLoadProvider{out: want}
	svc := New(p)
	if got := svc.DiagnoseLoad("query q { }", ""); got != nil {
		t.Errorf("an untitled document cannot be placed in the tree, so there is no pass: %+v", got)
	}
	if len(p.calls) != 0 {
		t.Errorf("the engine must not be asked without a path: %v", p.calls)
	}
	if got := svc.DiagnoseLoad("query q { }", "planner/queries.memql"); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want the engine's %+v", got, want)
	}
	if !reflect.DeepEqual(p.calls, []string{"planner/queries.memql"}) {
		t.Errorf("the engine is asked with the document's tree path: %v", p.calls)
	}

	// A provider with no engine behind it, and no provider at all.
	if got := New(nil).DiagnoseLoad("query q { }", "planner/queries.memql"); got != nil {
		t.Errorf("a registry-less service runs no load: %+v", got)
	}
	var plain RegistryProvider = struct{ RegistryProvider }{}
	if got := New(plain).DiagnoseLoad("query q { }", "planner/queries.memql"); got != nil {
		t.Errorf("a provider that cannot load runs no load: %+v", got)
	}
}
