package sense

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDiagnose_ReferenceFiles_NoErrors gates the dsl/_reference/*.memql
// authoring skeletons -- the documented source of truth for each construct's
// syntax -- through sense.Diagnose, the entry point the cockpit editor runs
// when a user opens one. Like the sibling TestDiagnose_EmbeddedTree_NoErrors it
// uses a registry-less service (New(nil)), so it gates the rewrite/lex/parse
// layer -- exactly the rot this targets (misplaced imports, a field missing its
// type) -- and not the registry-gated semantic checks a live editor also runs.
//
// These files are deliberately outside every OTHER gate: go:embed omits
// _reference/, the dslfs walker skips underscore paths, and
// TestDiagnose_EmbeddedTree_NoErrors explicitly skips _reference/ because the
// engine never loads them. That left nothing holding them syntactically valid,
// so they rotted (misplaced `use` imports that belong in the file-top preamble;
// a @variant field declared without its type) -- and the rot only surfaced as
// parse errors in the editor when someone opened one. This test closes that
// gap: the syntax source-of-truth must itself parse clean. (memql#2748)
func TestDiagnose_ReferenceFiles_NoErrors(t *testing.T) {
	root := dslRoot(t)
	svc := New(nil)

	files, err := filepath.Glob(filepath.Join(root, "_reference", "*.memql"))
	if err != nil {
		t.Fatalf("glob _reference: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no _reference/*.memql files found under %s", root)
	}

	for _, f := range files {
		f := f
		rel := filepath.Join("_reference", filepath.Base(f))
		t.Run(filepath.Base(f), func(t *testing.T) {
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			errs := errorDiags(svc.Diagnose(string(src), rel))
			for _, d := range errs {
				t.Errorf("%s L%d:C%d [%s] %s", rel, d.Range.Start.Line, d.Range.Start.Column, d.Code, d.Message)
			}
			if len(errs) != 0 {
				t.Fatalf("%s: expected 0 error diagnostics, got %d", rel, len(errs))
			}
		})
	}
}
