package dslgate

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// factsCorpus is a small corpus that reaches every gate the facts feed: a
// flagged query, a cross-namespace call, a statement body calling a builtin,
// a late `use` line, an automation outside automations.memql, a bare row
// intrinsic in a filter, and two imports binding one local name.
//
// Each text carries marker, so a test that needs texts the process has not
// seen before gets them.
func factsCorpus(marker string) []SourceFile {
	return []SourceFile{
		{Path: "probe/queries.memql", Content: `// ` + marker + `
use probe.concepts.{ thing }

query thing byOwner {
  args {
    owner string
  }
  filter row => row.ownerUserId == args.owner
  paginate 10
}

query thing byId {
  args {
    id string
  }
  filter row => id == args.id
  paginate 10
}
`},
		{Path: "product/queries.memql", Content: `// ` + marker + `
use product.concepts.{ widget }

query widget runtimeQuery {
  args {
    id string
  }
  filter row => row.id == args.id
  paginate 10
}
`},
		{Path: "core/logic.memql", Content: `// ` + marker + `
use core.concepts.{ gadget }
use other.concepts.{ gadget }

logic callsRuntime {
  args {
    id string
  }
  found := query runtimeQuery(id: args.id)
  return found
}

automation misplacedHere {
  builtin note(v: 1)
}

use late.concepts.{ thing }
`},
	}
}

// scanFacts is a scan's violations, one line each, for comparing two scans.
func scanFacts(files []SourceFile, opts Options) []string {
	var out []string
	for _, v := range ScanFiles(files, opts) {
		out = append(out, v.String())
	}
	return out
}

// TestSourceFactsDeriveOncePerText: a second scan of the same texts derives
// nothing -- every fact comes from the memo -- and reports exactly what the
// first did. Asserted as a COUNT of derivations rather than as a duration,
// the way TestSharedDblessEngine_SharesOneBoot asserts its sharing: a
// regression that re-derived per scan would still be fast on a fast machine.
func TestSourceFactsDeriveOncePerText(t *testing.T) {
	corpus := factsCorpus(t.Name())
	opts := Options{BuiltinStepRefusal: func(string, string) (string, bool) { return "", true }}

	before := sourceFactsDerived.Load()
	first := scanFacts(corpus, opts)
	derived := sourceFactsDerived.Load() - before
	if derived != int64(len(corpus)) {
		t.Fatalf("the first scan derived facts for %d texts, want %d -- one per distinct text", derived, len(corpus))
	}
	if len(first) == 0 {
		t.Fatal("the corpus reached no gate; the test would pass over a scan that read nothing")
	}

	again := sourceFactsDerived.Load()
	second := scanFacts(corpus, opts)
	if d := sourceFactsDerived.Load() - again; d != 0 {
		t.Errorf("the second scan of the same texts derived facts %d times, want 0", d)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("the second scan disagrees with the first:\nfirst:\n%s\nsecond:\n%s",
			strings.Join(first, "\n"), strings.Join(second, "\n"))
	}
}

// TestSourceFactsNeverHoldAVerdict: the memo holds what the TEXT decides and
// nothing else. The same texts scanned under different Options, and at
// different paths, get each scan's own answer -- in both directions, and back
// again, so a verdict cached by the first scan would show on the third.
func TestSourceFactsNeverHoldAVerdict(t *testing.T) {
	corpus := factsCorpus(t.Name())
	has := func(vs []Violation, gate Gate, construct string) bool {
		for _, v := range vs {
			if v.Gate == gate && (construct == "" || v.Construct == construct) {
				return true
			}
		}
		return false
	}
	crossNamespace := func(vs []Violation) bool {
		for _, v := range vs {
			if v.Gate == GateCrossNamespaceImport && strings.Contains(v.Detail, `"runtimeQuery"`) {
				return true
			}
		}
		return false
	}

	serverOnly := Options{ServerOnly: func(file, name string) bool { return file == "probe/queries.memql" && name == "byOwner" }}
	core := Options{CoreDomain: func(domain string) bool { return domain == "core" }}
	refuses := Options{BuiltinStepRefusal: func(name, _ string) (string, bool) {
		if name == "note" {
			return "it is refused here.", true
		}
		return "", false
	}}
	accepts := Options{BuiltinStepRefusal: func(string, string) (string, bool) { return "", true }}

	for i, step := range []struct {
		opts                    Options
		flagged, cross, builtin bool
	}{
		{Options{}, true, true, false},
		{serverOnly, false, true, false},
		{core, true, false, false},
		{refuses, true, true, true},
		{accepts, true, true, false},
		{Options{}, true, true, false},
	} {
		got := ScanFiles(corpus, step.opts)
		if f := has(got, GateUserScopeSelection, "byOwner"); f != step.flagged {
			t.Errorf("scan %d: byOwner flagged = %v, want %v -- the @serverOnly verdict is the caller's, on every scan", i, f, step.flagged)
		}
		if c := crossNamespace(got); c != step.cross {
			t.Errorf("scan %d: runtimeQuery cross-namespace report = %v, want %v -- the core-domain verdict is the caller's", i, c, step.cross)
		}
		if b := has(got, GateBuiltinStepArgs, ""); b != step.builtin {
			t.Errorf("scan %d: builtin-step report = %v, want %v -- the builtin profile is the caller's", i, b, step.builtin)
		}
	}

	// The same text at two paths: the placement verdict is the path's, and a
	// per-file finding names the file it was scanned as.
	logic := corpus[2].Content
	for _, tc := range []struct {
		path      string
		misplaced bool
	}{
		{"core/logic.memql", true},
		{"core/automations.memql", false},
		{"core/logic.memql", true},
	} {
		got := ScanSource(tc.path, logic, Options{})
		if m := has(got, GateConstructMisplaced, "misplacedHere"); m != tc.misplaced {
			t.Errorf("%s: construct-misplaced = %v, want %v", tc.path, m, tc.misplaced)
		}
		if !has(got, GateMisplacedUse, "") || !has(got, GateDuplicateImportName, "gadget") {
			t.Fatalf("%s: the per-file gates did not report the late use line and the duplicate import:\n%v", tc.path, got)
		}
		for _, v := range got {
			if v.File != tc.path {
				t.Errorf("a finding scanned as %s names %s: %s", tc.path, v.File, v)
			}
		}
	}
}

// TestStampedFindingsAreTheCallersCopy: what a gate hands out is a copy -- a
// caller that edits it cannot change what the next caller is told.
func TestStampedFindingsAreTheCallersCopy(t *testing.T) {
	src := factsCorpus(t.Name())[2].Content
	first := scanMisplacedUseLines("a/logic.memql", src)
	if len(first) != 1 {
		t.Fatalf("want one late use line, got %v", first)
	}
	want := first[0].Detail
	first[0].Detail = "edited by a caller"
	first[0].Line = -1

	second := scanMisplacedUseLines("b/logic.memql", src)
	if len(second) != 1 || second[0].Detail != want || second[0].Line < 1 || second[0].File != "b/logic.memql" {
		t.Errorf("the second caller got %+v, want the derived finding stamped with b/logic.memql", second)
	}
}

// TestStatementBodiesAreParsedPerScan: the memo holds where a declaration's
// text is, never its AST -- an AST is mutable, and two scans sharing one would
// share whatever either did to it.
func TestStatementBodiesAreParsedPerScan(t *testing.T) {
	files := factsCorpus(t.Name())[2:3]
	collect := func() map[string]*ast.AutomationDef {
		out := map[string]*ast.AutomationDef{}
		eachStatementBody(files, func(_ SourceFile, kind, name string, _ int, def *ast.AutomationDef) {
			out[kind+" "+name] = def
		})
		return out
	}
	first, second := collect(), collect()
	if len(first) != 2 {
		t.Fatalf("want the logic and the automation, got %v", first)
	}
	for key, def := range first {
		if second[key] == def {
			t.Errorf("%s: two scans were handed the same AST %p", key, def)
		}
	}
}

// TestSourceFactsMemoIsBounded: a text larger than the bound is derived and
// not held, and the memo clears rather than growing past it.
func TestSourceFactsMemoIsBounded(t *testing.T) {
	big := strings.Repeat("// padding\n", sourceFactsMaxBytes/len("// padding\n")+1)
	if len(big) <= sourceFactsMaxBytes {
		t.Fatalf("fixture is %d bytes, not over the %d bound", len(big), sourceFactsMaxBytes)
	}
	if got := factsOf(big); got == factsOf(big) {
		t.Error("a text over the bound was held")
	}

	sourceFactsMemo.mu.Lock()
	held := sourceFactsMemo.bytes
	sourceFactsMemo.mu.Unlock()
	if held > sourceFactsMaxBytes {
		t.Errorf("the memo holds %d bytes of text, over its %d bound", held, sourceFactsMaxBytes)
	}
	small := fmt.Sprintf("// %s\nquery thing q {\n}\n", t.Name())
	if factsOf(small) != factsOf(small) {
		t.Error("a small text was not held")
	}
}
