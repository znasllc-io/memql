package memql_test

// authoring_line_diagnostics_test.go -- E4 (epic memql#2354, issue #2375): a
// Gate-1 bundle parse failure carries the AUTHORED bundle-file line/column, and
// the position is OMITTED (never guessed) when it cannot be mapped reliably.
//
// External test package so it can blank-import component/automations, whose
// init() registers the automation-compile hook the sandbox uses for the
// `automation` kind (without it the automation would report as skipped).

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"

	_ "github.com/znasllc-io/memql/component/automations"
)

// authoredLineOf returns the 1-based bundle line where marker first appears.
func authoredLineOf(t *testing.T, bundle, marker string) int {
	t.Helper()
	idx := strings.Index(bundle, marker)
	if idx < 0 {
		t.Fatalf("marker %q not found in bundle", marker)
	}
	return 1 + strings.Count(bundle[:idx], "\n")
}

// diagFor returns the per-construct diagnostic for name, or fails.
func diagFor(t *testing.T, bundle, name string) memql.SandboxDiagnostic {
	t.Helper()
	report := memql.SandboxCompileBundle(memql.SplitBundleSource(bundle))
	for _, d := range report.Diagnostics {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("no diagnostic for construct %q; report=%+v", name, report.Diagnostics)
	return memql.SandboxDiagnostic{}
}

// A valid prior construct offsets the erroring construct deep into the bundle so
// BundleLine is a real >1 anchor -- an identity mapping would fail these.
const priorConcept = `@version("1.0.0")
@namespace("probe")
@description("probe widget")
concept probeWidget {
  ownerUserId string @required
}`

// TestBundleDiagnostic_AuthoredLine covers the four kinds the E4 acceptance
// names -- query + logic (NormaliseAll-lowered) and automation + action
// (struct-form / re-sliced) -- proving the parse position maps back to the
// AUTHORED bundle line the offending token sits on.
func TestBundleDiagnostic_AuthoredLine(t *testing.T) {
	prefix := priorConcept + "\n\n"

	cases := []struct {
		kind   string
		name   string
		src    string
		marker string // authored substring on the offending line
		// exactCol asserts a real column (the failing token's column is on a
		// line the lowering preserved byte-identically).
		exactCol bool
	}{
		{
			kind: "query", name: "queryBad", marker: "id 999", exactCol: true,
			src: `@description("bad query")
query probeWidget queryBad {
  args {
    id 999 @required
  }
  filter ownerUserId == args.id
  shape whatever
}`,
		},
		{
			kind: "logic", name: "logicBad", marker: "args.x $", exactCol: true,
			src: `@description("bad logic")
logic logicBad {
  args {
    x string @required
  }
  body {
    return coalesce(args.x $ "y")
  }
}`,
		},
		{
			kind: "action", name: "actionBad", marker: "readFile(path: $", exactCol: true,
			src: `use capabilities.fs.{ readFile }

@description("bad action")
action actionBad {
  args {
    path string @required
  }
  capability readFile(path: $ )
}`,
		},
		{
			kind: "automation", name: "autoBad", marker: "host: $", exactCol: true,
			src: `@trigger(event="deploy.requested", concept="v1:cluster:deployment", partition="*")
automation autoBad {
  step decide {
    mutation createDatabase (
      host: $ )
  }
}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			bundle := prefix + tc.src
			want := authoredLineOf(t, bundle, tc.marker)
			d := diagFor(t, bundle, tc.name)

			if d.OK {
				t.Fatalf("%s %q unexpectedly compiled OK", tc.kind, tc.name)
			}
			if d.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", d.Kind, tc.kind)
			}
			if d.Line != want {
				t.Errorf("authored line = %d, want %d (the %q line). err=%q", d.Line, want, tc.marker, d.Error)
			}
			// The mapped line must sit strictly deeper than the construct's own
			// first line -- proof the bundle anchor (not identity) was applied.
			if d.Line <= 1 {
				t.Errorf("authored line %d is not offset into the bundle (anchor not applied)", d.Line)
			}
			if tc.exactCol && d.Column <= 0 {
				t.Errorf("expected an exact column on a preserved line, got %d", d.Column)
			}
			if d.EndLine != 0 && d.EndLine < d.Line {
				t.Errorf("endLine %d precedes line %d", d.EndLine, d.Line)
			}
		})
	}
}

// TestBundleDiagnostic_OffsetShiftInvariance proves the mapping is truly anchored
// to the bundle: the SAME erroring construct pushed further down the bundle
// reports a line shifted by exactly the number of lines inserted before it.
func TestBundleDiagnostic_OffsetShiftInvariance(t *testing.T) {
	erroring := `@description("bad logic")
logic logicShift {
  args {
    x string @required
  }
  body {
    return coalesce(args.x $ "y")
  }
}`
	base := priorConcept + "\n\n" + erroring
	// Insert N extra blank lines between the prior construct and the erroring one.
	const extra = 5
	shifted := priorConcept + "\n\n" + strings.Repeat("\n", extra) + erroring

	dBase := diagFor(t, base, "logicShift")
	dShift := diagFor(t, shifted, "logicShift")
	if dBase.Line == 0 || dShift.Line == 0 {
		t.Fatalf("expected positions on both bundles, got base=%d shifted=%d", dBase.Line, dShift.Line)
	}
	if got := dShift.Line - dBase.Line; got != extra {
		t.Errorf("line shift = %d, want %d (base=%d shifted=%d)", got, extra, dBase.Line, dShift.Line)
	}
}

// TestBundleDiagnostic_OmittedWhenNoAnchor covers the omit path: a construct fed
// directly (the planner / DB-row path) carries no bundle offset, so a parse
// failure reports the error text but ZERO position -- never a guessed line.
func TestBundleDiagnostic_OmittedWhenNoAnchor(t *testing.T) {
	// Hand-built SandboxConstruct with no BundleLine -- exactly what the
	// non-splitter callers pass.
	report := memql.SandboxCompileBundle([]memql.SandboxConstruct{{
		Kind: "logic",
		Name: "noAnchor",
		Source: `@description("bad")
logic noAnchor {
  args {
    x string @required
  }
  body {
    return coalesce(args.x $ "y")
  }
}`,
	}})
	for _, dd := range report.Diagnostics {
		if dd.Name == "noAnchor" {
			if dd.OK {
				t.Fatal("expected noAnchor to fail")
			}
			if dd.Error == "" {
				t.Error("expected a non-empty error message")
			}
			if dd.Line != 0 || dd.Column != 0 || dd.EndLine != 0 || dd.EndColumn != 0 {
				t.Errorf("expected omitted position (all zero), got line=%d col=%d endLine=%d endCol=%d",
					dd.Line, dd.Column, dd.EndLine, dd.EndColumn)
			}
			return
		}
	}
	t.Fatal("no diagnostic for noAnchor")
}

// TestBundleDiagnostic_OmittedForNonParseFailure covers the second omit path: a
// failure with no recoverable parser position (here concept resolution against
// a concept absent from both the core registry and the bundle) reports the error
// text but ZERO position.
func TestBundleDiagnostic_OmittedForNonParseFailure(t *testing.T) {
	// Valid syntax; binds a signature concept that does not exist -> resolution
	// error, which carries no ParseError position.
	bundle := `@description("dangling concept")
query doesNotExistConcept queryDangling {
  args {
    id string @required
  }
  filter ownerUserId == args.id
  shape whatever
}`
	d := diagFor(t, bundle, "queryDangling")
	if d.OK {
		t.Fatal("expected queryDangling to fail concept resolution")
	}
	if d.Line != 0 || d.Column != 0 {
		t.Errorf("expected omitted position for a non-parse failure, got line=%d col=%d (err=%q)", d.Line, d.Column, d.Error)
	}
}
