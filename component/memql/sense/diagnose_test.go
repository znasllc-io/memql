package sense

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dslRoot resolves the repo-root dsl/ tree relative to this package
// directory (component/memql/sense -> repo root is three levels up).
func dslRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "dsl"))
	if err != nil {
		t.Fatalf("resolving dsl root: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("dsl root not found at %s: %v", root, err)
	}
	return root
}

// errorDiags returns only the SeverityError diagnostics.
func errorDiags(diags []Diagnostic) []Diagnostic {
	var out []Diagnostic
	for _, d := range diags {
		if d.Severity == SeverityError {
			out = append(out, d)
		}
	}
	return out
}

// TestDiagnose_ValidConstructKindFiles_NoErrors is the direct regression for
// memql#2359: every construct-kind file in the DevOps deployment bundle, the
// cluster query file that surfaced the bug, and a real cognition automations
// file must diagnose with ZERO errors now that Diagnose lowers struct-form
// constructs through the real rewriter chain before parsing. A registry-less
// service isolates the rewrite + lex + parse path (Phase 3 semantic analysis
// is registry-gated), which is exactly the pipeline the bug lived in.
func TestDiagnose_ValidConstructKindFiles_NoErrors(t *testing.T) {
	root := dslRoot(t)
	svc := New(nil)

	files := []string{
		"deployment/actions.memql",
		"deployment/automations.memql",
		"deployment/builtins.memql",
		"deployment/concepts.memql",
		"deployment/logic.memql",
		"deployment/mutations.memql",
		"deployment/queries.memql",
		"deployment/shapes.memql",
		"deployment/specs.memql",
		"cluster/queries.memql",
		"cognition/automations.memql",
	}

	for _, rel := range files {
		rel := rel
		t.Run(rel, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			errs := errorDiags(svc.Diagnose(string(src), rel))
			if len(errs) != 0 {
				for _, d := range errs {
					t.Errorf("L%d:C%d [%s] %s", d.Range.Start.Line, d.Range.Start.Column, d.Code, d.Message)
				}
				t.Fatalf("%s: expected 0 errors, got %d", rel, len(errs))
			}
		})
	}
}

// TestDiagnose_EmbeddedTree_NoErrors sweeps EVERY .memql file in the tree.
// The engine loads them all at startup through the same lowering + parse, so
// each must diagnose clean; this is the broad guard behind the acceptance
// "Diagnose over every file in the embedded tree returns zero errors".
func TestDiagnose_EmbeddedTree_NoErrors(t *testing.T) {
	root := dslRoot(t)
	svc := New(nil)

	var scanned int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".memql") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		// dsl/_reference/ holds authoring skeletons (placeholder syntax) that
		// the engine never loads, so they are outside the "embedded tree that
		// must diagnose clean" contract.
		if strings.HasPrefix(rel, "_reference"+string(os.PathSeparator)) {
			return nil
		}
		scanned++
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Errorf("read %s: %v", rel, readErr)
			return nil
		}
		if errs := errorDiags(svc.Diagnose(string(src), rel)); len(errs) != 0 {
			for _, d := range errs {
				t.Errorf("%s L%d:C%d [%s] %s", rel, d.Range.Start.Line, d.Range.Start.Column, d.Code, d.Message)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk dsl tree: %v", err)
	}
	if scanned == 0 {
		t.Fatalf("scanned 0 .memql files under %s", root)
	}
	t.Logf("swept %d .memql files, all clean", scanned)
}

// TestDiagnose_StructFormDoesNotFalseError pins the exact symptom from the
// issue: a struct-form query file must NOT report the pre-fix
// `unexpected token "query"` parse error.
func TestDiagnose_StructFormDoesNotFalseError(t *testing.T) {
	src := `use cluster.concepts.{ node }

@enabled
@description("returns nodes")
@unbounded("bounded topology set consumed whole")
query node clusterNodes {
}
`
	diags := New(nil).Diagnose(src, "cluster/queries.memql")
	for _, d := range diags {
		if strings.Contains(d.Message, `unexpected token "query"`) {
			t.Fatalf("struct-form query false-errored: %s", d.Message)
		}
	}
	if errs := errorDiags(diags); len(errs) != 0 {
		t.Fatalf("expected clean diagnose, got %d errors: %+v", len(errs), errs)
	}
}

// TestDiagnose_InjectedErrors verifies that a corrupted construct reports a
// diagnostic anchored on the AUTHORED source line. `mapMode` records how the
// line map resolves the error: `exact` when the offending token survives the
// lowering verbatim (preamble / args-field / stray top-level token), or
// `within` when the error lands on synthesized scaffolding and resolves into
// the construct's authored line span.
func TestDiagnose_InjectedErrors(t *testing.T) {
	svc := New(nil)

	cases := []struct {
		name     string
		src      string
		wantLine int // expected authored line of the first error
		exactCol int // expected column when > 0 (verbatim-preserved lines only)
		wantCode string
	}{
		{
			// Bad token inside a query body -> lands on the synthesized
			// `return` line, which maps back into the construct (its header
			// line). Authored: header at line 5.
			// The lowered `return` line maps 1:1 with the authored `filter`
			// line (equal-length hunk), so the map resolves it to line 6 --
			// the actual offending body line, not just the construct header.
			name: "bad-token-in-query-body",
			src: "use cluster.concepts.{ node }\n" + // 1
				"\n" + //                                2
				"@enabled\n" + //                        3
				"@description(\"bad\")\n" + //            4
				"query node badBody {\n" + //            5  <- construct header
				"  filter  health != @@@bogus@@@\n" + // 6  <- offending body line
				"}\n", //                                 7
			wantLine: 6,
			wantCode: "parse-error",
		},
		{
			// Legacy object-literal args syntax -> parse error on the
			// args-field line, which the rewriter hoists VERBATIM, so the map
			// resolves it exactly (line + column).
			name: "legacy-object-literal-args",
			src: "@description(\"legacy args\")\n" + //           1
				"logic legacyOne {\n" + //                       2
				"  args {\n" + //                                3
				"    x: { type: \"string\", required: true }\n" + // 4  <- offending line
				"  }\n" + //                                      5
				"  body {\n" + //                                6
				"    return x\n" + //                            7
				"  }\n" + //                                      8
				"}\n", //                                         9
			wantLine: 4,
			exactCol: 6,
			wantCode: "parse-error",
		},
		{
			// Stray top-level garbage between two valid constructs -> the
			// token is preserved verbatim, so the error maps to its exact
			// authored line/column.
			name: "stray-top-level-token",
			src: "use cluster.concepts.{ node }\n" + //   1
				"\n" + //                                  2
				"@enabled\n" + //                          3
				"query node goodA {\n" + //                4
				"  filter health != \"stopped\"\n" + //    5
				"}\n" + //                                 6
				"\n" + //                                  7
				"%%%garbage%%%\n" + //                     8  <- stray token
				"\n" + //                                  9
				"@enabled\n" + //                          10
				"query node goodB {\n" + //                11
				"}\n", //                                   12
			wantLine: 8,
			exactCol: 1,
			wantCode: "parse-error",
		},
		{
			// Logic missing its mandatory body { } block -> a LOWERING error
			// (no line/col of its own); the diagnostic anchors on the named
			// construct's authored header line.
			name: "missing-logic-body",
			src: "@description(\"no body\")\n" + //  1
				"logic doThing {\n" + //            2  <- construct header
				"  args {\n" + //                   3
				"    x string @required\n" + //     4
				"  }\n" + //                         5
				"}\n", //                            6
			wantLine: 2,
			wantCode: "rewrite-error",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			errs := errorDiags(svc.Diagnose(tc.src, "test.memql"))
			if len(errs) == 0 {
				t.Fatalf("expected at least one error, got none")
			}
			d := errs[0]
			if d.Range.Start.Line != tc.wantLine {
				t.Errorf("line = %d, want %d (msg: %s)", d.Range.Start.Line, tc.wantLine, d.Message)
			}
			if tc.exactCol > 0 && d.Range.Start.Column != tc.exactCol {
				t.Errorf("column = %d, want %d (msg: %s)", d.Range.Start.Column, tc.exactCol, d.Message)
			}
			if tc.wantCode != "" && d.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", d.Code, tc.wantCode)
			}
			// Every reported position must be inside the authored source --
			// "never worse than no position".
			nLines := strings.Count(tc.src, "\n") + 1
			if d.Range.Start.Line < 1 || d.Range.Start.Line > nLines {
				t.Errorf("line %d out of authored range [1,%d]", d.Range.Start.Line, nLines)
			}
		})
	}
}

// TestDiagnose_EmptyAndWhitespace covers the empty / whitespace-only edge
// cases: no diagnostics, no panic.
func TestDiagnose_EmptyAndWhitespace(t *testing.T) {
	svc := New(nil)
	for _, src := range []string{"", "   ", "\n\n\t\n", "  \n  \t  \n"} {
		if diags := svc.Diagnose(src, "empty.memql"); len(diags) != 0 {
			t.Errorf("Diagnose(%q) = %d diagnostics, want 0", src, len(diags))
		}
	}
}
