package dslimports

// v1_lanes_test.go -- lanes 5 and 6 on edition-2026 predicates (epic
// memql#5363, task memql#5368).
//
// The pushdown positions parse in both editions side by side (memql#5364): a
// v1 filter reaches the lanes as a LambdaExpr joined under the concept term,
// and a brace-less spec as SpecDecl.Lambda with Body nil. What must hold: the
// head of a v1 field reference is the FIELD after the parameter, never the
// parameter itself, a literal compared against an enum is still checked in its
// new spelling, and a spec whose body is a lambda is still walked. The first
// tests exercise the walks on the v1 parser's tree directly; the last two go
// through Load, the real file parser.

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

func v1Lambda(t *testing.T, src string) *languageAst.LambdaExpr {
	t.Helper()
	lam, err := languageParser.ParseV1Lambda(src)
	if err != nil {
		t.Fatalf("ParseV1Lambda(%q): %v", src, err)
	}
	return lam
}

func TestFilterFieldHeadsReadsAV1Lambda(t *testing.T) {
	lam := v1Lambda(t, `row => row.status == args.status
		&& (args.x == nil || row.ownerUserId == actor.userId)
		&& row.lineage.?planId != nil
		&& row.items.any(i => i.kind == "x")
		&& isActiveRecord(row)`)
	want := []string{"status", "ownerUserId", "lineage", "items"}
	// Wherever the lambda sits: bare, or under a directive wrapper.
	for _, n := range []languageAst.Node{lam, &languageAst.ShapeExpr{Target: lam}} {
		if got := filterFieldHeads(n); !reflect.DeepEqual(got, want) {
			t.Errorf("filterFieldHeads = %v, want %v -- the head is the field after the parameter; `args`, `actor`, a nested lambda's `i` and the parameter itself are not the row's fields", got, want)
		}
	}
}

func TestFilterEnumViolationsReadsAV1Lambda(t *testing.T) {
	enums := map[string][]string{"utteranceType": {"speech", "text", "action", "system"}}
	cases := []struct {
		name, src string
		want      []string // "<value>:<consequence prefix>"
	}{
		// CATCH, in every spelling the codemod produces.
		{"in list", `row => row.utteranceType in ["speech", "agentGreeting"]`, []string{"agentGreeting:the predicate is always false"}},
		{"equality", `row => row.utteranceType == "greeting"`, []string{"greeting:the predicate is always false"}},
		{"reversed equality", `row => "greeting" == row.utteranceType`, []string{"greeting:the predicate is always false"}},
		{"inequality", `row => row.utteranceType != "greeting"`, []string{"greeting:the predicate is always true"}},
		{"negated membership", `row => !(row.utteranceType in ["greeting"])`, []string{"greeting:the predicate is always true"}},
		{"on a wrapped line", "row => row.agentId == args.agentId\n          && row.utteranceType == \"greeting\"", []string{"greeting:the predicate is always false"}},
		// PASS.
		{"members", `row => row.utteranceType in ["speech", "text"]`, nil},
		{"an argument is unknowable here", `row => row.utteranceType == args.kind`, nil},
		{"not the row's field", `row => args.utteranceType == "greeting"`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, v := range filterEnumViolations(v1Lambda(t, tc.src), enums) {
				got = append(got, v.value+":"+v.consequence)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("violations = %v, want %v", got, tc.want)
			}
			for i := range got {
				if !strings.HasPrefix(got[i], tc.want[i]) {
					t.Errorf("violation %d = %q, want prefix %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestSpecBodyHeadsReadAV1Lambda: lane 6 reads a spec body through the same
// walk, and an edition-2026 spec over an @actor shape names its parameter
// `actor` -- whose field, not whose name, is what the shape must project.
func TestSpecBodyHeadsReadAV1Lambda(t *testing.T) {
	if got := filterFieldHeads(v1Lambda(t, `actor => actor.role == "owner" || actor.isClusterOwner == true`)); !reflect.DeepEqual(got, []string{"role", "isClusterOwner"}) {
		t.Errorf("heads = %v, want [role isClusterOwner]", got)
	}
}

// TestSpecBodyLaneReadsABraceLessSpec goes through the real parser: an
// edition-2026 spec's body arrives as SpecDecl.Lambda with Body nil, and the
// lane keyed on Body -- so it skipped every migrated spec while its coverage
// probe counted them as checked.
func TestSpecBodyLaneReadsABraceLessSpec(t *testing.T) {
	// CATCH: a misspelled envelope key, and a misspelled concept property.
	errs := specBodyErrs(t, `use lab.shapes.{ labActor }
use lab.concepts.{ widget }

/// Caller must be an owner -- with a typo'd envelope key.
spec labActor requiresOwnerV1 = actor => actor.rolle == "owner"

/// Rows in a region -- with a typo'd property, on a wrapped line.
spec widget inRegionV1 = row => row.ownerUserId != ""
                         && row.regoin == "eu"
`)
	if len(errs) != 2 {
		t.Fatalf("expected 2 spec-body errors, got %d: %v", len(errs), errs)
	}
	joined := errs[0].Error() + "\n" + errs[1].Error()
	for _, want := range []string{`"rolle"`, `"regoin"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("the lane did not report %s:\n%s", want, joined)
		}
	}
	// PASS: the same two specs, spelled right, including a row intrinsic a
	// concept-bound spec may name.
	if errs := specBodyErrs(t, `use lab.shapes.{ labActor }
use lab.concepts.{ widget }

/// Caller is an owner.
spec labActor requiresOwnerV1 = actor => actor.role == "owner"

/// Rows in a region, created after the epoch.
spec widget inRegionV1 = row => row.region == "eu" && row.createdAt > "1970-01-01"
`); len(errs) != 0 {
		t.Errorf("a correctly spelled brace-less spec was reported: %v", errs)
	}
}

// TestFilterLaneReadsAV1Filter goes through the real parser too: a v1 filter
// reaches the lane as a LambdaExpr joined under the concept term.
func TestFilterLaneReadsAV1Filter(t *testing.T) {
	tree := specBodyTree("")
	tree["lab/shapes.memql"] = &fstest.MapFile{Data: []byte(
		"/// Actor identity envelope.\n@actor\nshape labActor {\n  actor.userId\n  actor.role\n}\n\n" +
			"/// A widget card.\n@row\nshape widget widgetCard {\n  row.id\n  region\n}\n")}
	tree["lab/queries.memql"] = &fstest.MapFile{Data: []byte(`use lab.concepts.{ widget }

/// CATCH: a typo'd field on a wrapped line.
@unbounded("fixture")
query widget widgetsTypo {
  args {
    region  string
  }
  filter  row => row.ownerUserId == actor.userId
              && row.regoin == args.region
  shape   widgetCard
}

/// PASS.
@unbounded("fixture")
query widget widgetsFine {
  args {
    region  string
  }
  filter  row => row.ownerUserId == actor.userId && (args.region == nil || row.region == args.region)
  shape   widgetCard
}
`)}
	loaded, err := Load(tree)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var lane5 []string
	for _, e := range loaded.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "filter compares field") {
			lane5 = append(lane5, e.Error())
		}
	}
	if len(lane5) != 1 || !strings.Contains(lane5[0], `"regoin"`) || !strings.Contains(lane5[0], "widgetsTypo") {
		t.Fatalf("lane 5 findings = %v, want exactly the typo in widgetsTypo", lane5)
	}
}
