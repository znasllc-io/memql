package memoryNodes

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// buildConcept parses a single-concept source and builds it, the way
// the unified loader does.
func buildConcept(t *testing.T, src string) (*Concept, error) {
	t.Helper()
	file, err := langparser.ParseFile(src)
	if err != nil {
		t.Fatalf("ParseFile:\n%s\nerror: %v", src, err)
	}
	for _, def := range file.Definitions {
		if cd, ok := def.(*langparser.ConceptDecl); ok {
			return BuildConceptFromDecl(cd, "v1:test:probe")
		}
	}
	t.Fatalf("no concept declaration in:\n%s", src)
	return nil, nil
}

const rowAuthzProbeBody = ` probe {
  ownerUserId  string  @required
  name         string
}
`

// Each of the four tiers loads, and lands on the built Concept with
// the meaning it was declared with.
func TestConceptLoadsEachRowAuthzTier(t *testing.T) {
	cases := []struct {
		annotation string
		want       langparser.RowAuthzDecl
	}{
		{`@rowAuthz(public)`, langparser.RowAuthzDecl{Tier: langparser.RowAuthzPublic}},
		{`@rowAuthz(clusterOwner)`, langparser.RowAuthzDecl{Tier: langparser.RowAuthzClusterOwner}},
		{`@rowAuthz(owner="ownerUserId")`, langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"}},
		{`@rowAuthz(via="spaceMember")`, langparser.RowAuthzDecl{Tier: langparser.RowAuthzGranted, Spec: "spaceMember"}},
	}
	for _, tc := range cases {
		t.Run(tc.annotation, func(t *testing.T) {
			c, err := buildConcept(t, tc.annotation+"\nconcept"+rowAuthzProbeBody)
			if err != nil {
				t.Fatalf("BuildConceptFromDecl(%s): %v", tc.annotation, err)
			}
			if c.RowAuthz == nil {
				t.Fatalf("BuildConceptFromDecl(%s): RowAuthz is nil", tc.annotation)
			}
			if *c.RowAuthz != tc.want {
				t.Fatalf("BuildConceptFromDecl(%s): RowAuthz = %+v, want %+v", tc.annotation, *c.RowAuthz, tc.want)
			}
		})
	}
}

// A concept with no declaration loads and carries no tier. Phase 1
// warns; it does not refuse.
func TestConceptWithoutRowAuthzStillLoads(t *testing.T) {
	c, err := buildConcept(t, "concept"+rowAuthzProbeBody)
	if err != nil {
		t.Fatalf("BuildConceptFromDecl: %v", err)
	}
	if c.RowAuthz != nil {
		t.Fatalf("RowAuthz = %+v, want nil for an undeclared concept", c.RowAuthz)
	}
}

// Every malformed declaration refuses to load, and the diagnostic
// names the concept and the problem.
func TestConceptRejectsMalformedRowAuthz(t *testing.T) {
	cases := []struct {
		name       string
		annotation string
		wantHint   string
	}{
		{"unknown tier", `@rowAuthz(everyone)`, `unknown tier "everyone"`},
		{"no tier", `@rowAuthz()`, "requires a tier"},
		{"bare value", `@rowAuthz("public")`, "does not take a bare value"},
		{"two tiers", `@rowAuthz(public, clusterOwner)`, "takes exactly one tier"},
		{"empty owner", `@rowAuthz(owner="")`, "is empty"},
		{"owner names an undeclared field", `@rowAuthz(owner="noSuchField")`, "does not declare"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildConcept(t, tc.annotation+"\nconcept"+rowAuthzProbeBody)
			if err == nil {
				t.Fatalf("BuildConceptFromDecl(%s): want a load error, got nil", tc.annotation)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantHint) {
				t.Fatalf("error = %q, want it to contain %q", msg, tc.wantHint)
			}
			if !strings.Contains(msg, "v1:test:probe") && !strings.Contains(msg, "probe") {
				t.Fatalf("error = %q, want it to name the concept", msg)
			}
		})
	}
}

// Two declarations on one concept must refuse to load rather than
// silently last-win. The parser folds attributes in source order, so
// without this a reader scanning top-down sees a tier the engine does
// not use.
func TestConceptRejectsTwoRowAuthzDeclarations(t *testing.T) {
	sources := []string{
		"@rowAuthz(public)\n@rowAuthz(owner=\"ownerUserId\")\nconcept" + rowAuthzProbeBody,
		"@rowAuthz(public)\n@rowAuthz(public)\nconcept" + rowAuthzProbeBody,
		// Same line -- the parser accepts several annotations per line.
		"@description(\"d\") @rowAuthz(public)\n@rowAuthz(clusterOwner)\nconcept" + rowAuthzProbeBody,
	}
	for _, src := range sources {
		_, err := buildConcept(t, src)
		if err == nil {
			t.Fatalf("two declarations loaded without error:\n%s", src)
		}
		if !strings.Contains(err.Error(), "more than once") {
			t.Fatalf("error = %q, want it to say the annotation was declared more than once", err)
		}
	}
}

// The `owner=` field-existence check must name the fields that DO
// exist, so an author can fix a typo without opening the concept.
func TestRowAuthzOwnerErrorListsDeclaredFields(t *testing.T) {
	_, err := buildConcept(t, `@rowAuthz(owner="ownerUserID")`+"\nconcept"+rowAuthzProbeBody)
	if err == nil {
		t.Fatal("want a load error for a mis-cased owner field, got nil")
	}
	for _, want := range []string{"ownerUserId", "name"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to list declared field %q", err, want)
		}
	}
}

// THE AGREEMENT TEST (#2621's lesson, restated for #2920).
//
// The codemod's only emitter is FormatRowAuthz and the loader's only
// reader is ParseRowAuthz. If those two ever disagree, a codemod run
// writes a tree the loader refuses to boot. This asserts the whole
// path end to end: render a declaration the way the codemod does,
// insert it the way the codemod does, then load it the way the engine
// does, and require the meaning to survive.
func TestCodemodOutputLoadsWithTheSameMeaning(t *testing.T) {
	decls := []langparser.RowAuthzDecl{
		{Tier: langparser.RowAuthzPublic},
		{Tier: langparser.RowAuthzClusterOwner},
		{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"},
		{Tier: langparser.RowAuthzGranted, Spec: "spaceMember"},
	}
	for _, want := range decls {
		rewritten, err := langparser.RewriteRowAuthz(
			[]byte("concept"+rowAuthzProbeBody),
			map[string]langparser.RowAuthzDecl{"probe": want},
		)
		if err != nil {
			t.Fatalf("RewriteRowAuthz(%+v): %v", want, err)
		}
		c, err := buildConcept(t, string(rewritten))
		if err != nil {
			t.Fatalf("codemod emitted a tree the loader refuses (%+v):\n%s\nerror: %v", want, rewritten, err)
		}
		if c.RowAuthz == nil || *c.RowAuthz != want {
			t.Fatalf("codemod emitted %+v; loader read %+v", want, c.RowAuthz)
		}
	}
}

// Adding the annotation must not change the concept's JSON schema --
// the schema is what validation and filtering are built from, so a
// byte-identical schema is a direct statement that nothing downstream
// can behave differently because of the declaration.
func TestRowAuthzDoesNotChangeTheConceptSchema(t *testing.T) {
	plain, err := buildConcept(t, "concept"+rowAuthzProbeBody)
	if err != nil {
		t.Fatalf("undeclared concept: %v", err)
	}
	for _, annotation := range []string{
		`@rowAuthz(public)`,
		`@rowAuthz(clusterOwner)`,
		`@rowAuthz(owner="ownerUserId")`,
		`@rowAuthz(via="spaceMember")`,
	} {
		declared, err := buildConcept(t, annotation+"\nconcept"+rowAuthzProbeBody)
		if err != nil {
			t.Fatalf("%s: %v", annotation, err)
		}
		for key, want := range plain.Schemas {
			got, ok := declared.Schemas[key]
			if !ok {
				t.Fatalf("%s: schema %q disappeared", annotation, key)
			}
			if string(got) != string(want) {
				t.Fatalf("%s: schema %q changed\n got: %s\nwant: %s", annotation, key, got, want)
			}
		}
		if len(declared.Schemas) != len(plain.Schemas) {
			t.Fatalf("%s: schema set changed size", annotation)
		}
		// Everything else the executor reads off a Concept must be
		// identical too.
		if declared.NodeType != plain.NodeType || declared.Version != plain.Version ||
			len(declared.Relationships) != len(plain.Relationships) {
			t.Fatalf("%s: concept metadata changed", annotation)
		}
	}
}

// The declaration must round-trip through the Concept's JSON form,
// because that is how it reaches the cockpit's concept surface.
func TestRowAuthzSurvivesConceptJSON(t *testing.T) {
	c, err := buildConcept(t, `@rowAuthz(owner="ownerUserId")`+"\nconcept"+rowAuthzProbeBody)
	if err != nil {
		t.Fatalf("BuildConceptFromDecl: %v", err)
	}
	blob, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Concept
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.RowAuthz == nil || *back.RowAuthz != *c.RowAuthz {
		t.Fatalf("RowAuthz did not survive JSON: %+v -> %+v", c.RowAuthz, back.RowAuthz)
	}
	// An undeclared concept must not grow an empty object in its JSON.
	plain, err := buildConcept(t, "concept"+rowAuthzProbeBody)
	if err != nil {
		t.Fatalf("BuildConceptFromDecl: %v", err)
	}
	plainBlob, err := json.Marshal(plain)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(plainBlob), "rowAuthz") {
		t.Fatalf("undeclared concept serialises a rowAuthz key: %s", plainBlob)
	}
}

// TestRowAuthzIsInert is the gate that keeps row-authz from enforcing
// anything before someone decides it should.
//
// #2920 and #2921 are both explicit that no predicate is injected and
// no query result changes. The way that stops being true by accident is
// somebody reading Concept.RowAuthz from the execution path. This walks
// the Go tree and requires every reference to live in a file that is
// allowed to have one.
//
// The list GREW in #2921, and that growth is the mechanism working
// rather than the gate weakening: shadow mode necessarily reads the
// tier (that is what measuring means) and necessarily hooks the
// executor (that is where reads happen), so the files were moved on
// deliberately, in the commit that made the change. Phase 3 lands the
// same way.
//
// Reading is therefore no longer the invariant. The invariant that
// survives is that the tier never becomes part of a query, and it is
// asserted behaviourally elsewhere: `TestShadowHookDoesNotTouchTheExpression`
// (the hook does not mutate what it is handed) and
// `TestShadowModeChangesNoExpression` (the analyzer is pure).
func TestRowAuthzIsInert(t *testing.T) {
	root := repoRootForRowAuthz(t)

	// Files permitted to reference the row-authz surface: the shared
	// detector, the loader that fills the field, the boot-time
	// undeclared warning, the codemod, the annotation registry, and
	// (#2921) shadow mode's analyzer plus the two executor hook sites
	// that feed it. Anything else that names it is enforcement
	// arriving without a decision.
	allowed := map[string]bool{
		"component/language/parser/rowauthz_binding.go":     true,
		"component/database/memory-nodes/concept_parser.go": true,
		"component/database/memory-nodes/concept.go":        true,
		"component/language/annotations/registry.go":        true,
		"component/memql/unified_loader.go":                 true,
		"cmd/memqlmigrate/rowauthz_infer.go":                true,
		"cmd/memqlmigrate/main.go":                          true,
		// #2921 shadow mode: the analyzer, and the two hook sites that
		// feed it. The executor is on this list ONLY because it calls
		// recordShadow, which returns nothing and cannot alter a query.
		"component/memql/rowauthz_shadow.go": true,
		"component/memql/executor.go":        true,
		// memql#3079 Phase 5, the WRITE-side enforcement: the guard itself
		// and the single chokepoint that calls it. Added here per this gate's
		// own instruction ("If this is the enforcement phase landing, move the
		// file into the allow-list in the same commit"), so the change is
		// deliberate rather than incidental.
		//
		// This entry is expected to be short-lived: memql#3076 (Phase 3, read
		// side) RETIRES this whole test, because "no predicate is injected and
		// no query result changes" stops being true once either phase lands.
		// Whichever of the two merges second removes this list along with the
		// test. Kept correct on this branch rather than pre-emptively deleted,
		// so #3079 stands alone if it lands first.
		"component/memql/rowauthz_write.go":    true,
		"component/memql/executor_mutation.go": true,
	}

	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// `sdk/` is deliberately NOT skipped: sdk/gen is the
			// concept emitter, so it is one of the likeliest places
			// for the tier to start being read without anyone
			// noticing.
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if allowed[rel] {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !strings.Contains(string(src), "RowAuthz") {
			return nil
		}
		// Confirm it is a real identifier reference rather than the
		// word appearing in a comment or a string.
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, src, 0)
		if parseErr != nil {
			// Unparseable Go is not this test's business.
			return nil //nolint:nilerr // not a row-authz finding
		}
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if ok && strings.Contains(id.Name, "RowAuthz") {
				offenders = append(offenders, rel+": "+id.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("Phase 1 of #2920 is vocabulary only -- no predicate is injected and no query "+
			"result changes. These non-test files reference the row-authz surface and are not on "+
			"the allow-list:\n  %s\n\nIf this is the enforcement phase landing, move the file into "+
			"the allow-list in the same commit, so the change is deliberate rather than incidental.",
			strings.Join(offenders, "\n  "))
	}
}

// repoRootForRowAuthz walks up from the test's working directory to the
// module root (the directory holding go.mod).
func repoRootForRowAuthz(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find the module root (no go.mod above the test's working directory)")
		}
		dir = parent
	}
}
