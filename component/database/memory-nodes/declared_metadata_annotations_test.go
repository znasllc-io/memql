package memoryNodes

import (
	"context"
	"encoding/json"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/provenance"
)

// declared_metadata_annotations_test.go -- memql#2960.
//
// Four field annotations are DECLARED METADATA: @unique, @immutable, @secret
// and @default parse, reach the emitted schema, and nothing in the engine acts
// on them. The reference (dsl/_reference/_concept.memql section 8),
// docs/public/language/attribute-matrix.md and reserved.md all now say so
// plainly, having previously promised enforcement that does not exist.
//
// A documentation-only fix would repeat the very failure memql#2960 is an
// instance of -- a rule recorded somewhere and enforced nowhere. So the split
// is gated from both sides:
//
//   - the keys are still EMITTED (an author's declaration is not silently
//     dropped, and @pii's history shows a marker can become enforced later);
//   - the keys are still read by NOTHING, which is what makes the docs true.
//
// When enforcement lands for any of them, the second half fails and names the
// documents that have to change in the same commit. That is the point: the
// test is a tripwire on the docs, not an endorsement of the gap.

// buildFixtureConcept parses a single-concept fixture and builds it. The
// concept is picked out of File.Definitions rather than via
// component/memql.ExtractConceptDecls, which would be an import cycle from
// here.
func buildFixtureConcept(t *testing.T, src string) *Concept {
	t.Helper()
	file, err := parser.ParseFile(src)
	if err != nil {
		t.Fatalf("fixture did not parse: %v", err)
	}
	for _, def := range file.Definitions {
		decl, ok := def.(*parser.ConceptDecl)
		if !ok {
			continue
		}
		c, err := BuildConceptFromDecl(decl, "v1:ref:probe")
		if err != nil {
			t.Fatalf("fixture does not build: %v", err)
		}
		return c
	}
	t.Fatal("fixture declared no concept, so it measures nothing")
	return nil
}

// helper: build a concept from source and return its property schemas.
func propertySchemas(t *testing.T, src string) map[string]map[string]any {
	t.Helper()
	c := buildFixtureConcept(t, src)
	raw, err := c.DefinitionSchema()
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	var doc struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return doc.Properties
}

const declaredMetadataFixture = `@version("1.0.0")
@namespace("ref")
@description("d")
concept probe {
  label      string  @required @description("l")
  uniqueKey  string  @unique @description("u")
  externalId string  @immutable @description("i")
  apiKey     string  @secret @description("s")
  tier       string  @default("bronze") @description("t")
}
`

// The emission half. An author's declaration must survive into the schema --
// dropping it would make the annotation unrecoverable when enforcement lands,
// and @pii is the precedent for a marker graduating from declared to enforced
// without a schema change.
func TestDeclaredMetadataAnnotationsAreStillEmitted(t *testing.T) {
	props := propertySchemas(t, declaredMetadataFixture)
	for _, tc := range []struct{ field, key string }{
		{"uniqueKey", "x-unique"},
		{"externalId", "x-immutable"},
		{"apiKey", "x-secret"},
		{"tier", "default"},
	} {
		if _, ok := props[tc.field][tc.key]; !ok {
			t.Errorf("%s no longer emits %q. If the annotation was retired rather than left as "+
				"declared metadata, section 8 of dsl/_reference/_concept.memql still lists it "+
				"as accepted -- update it in the same change.\n  got: %v",
				tc.field, tc.key, props[tc.field])
		}
	}
}

// The reference sheet this issue rewrites is the page an author copies from,
// and NOTHING was building it. dsl/_reference/ is absent from dsl/embed.go's
// embed list and underscore-skipped by dslfs.WalkMemqlFiles, so every corpus
// gate structurally excludes it; the one test that does read it
// (component/memql/sense.TestDiagnose_ReferenceFiles_NoErrors) gates lex, parse
// and rewrite with a registry-less service and never reaches concept build.
//
// So a worked example could declare a reserved field, or misspell an
// annotation, and ship green -- teaching the author something the engine
// rejects. That is the same defect class memql#2960 exists to delete, one level
// up: the reference asserting behaviour that is not behaviour.
//
// This gate closes it by putting every concept in the sheet through
// BuildConceptFromDecl, which is the check that runs on a real domain.
func TestReferenceConceptSheetActuallyBuilds(t *testing.T) {
	path := filepath.Join(repoRoot(t), "dsl", "_reference", "_concept.memql")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read reference sheet: %v", err)
	}
	file, err := parser.ParseFile(string(src))
	if err != nil {
		t.Fatalf("dsl/_reference/_concept.memql did not parse: %v", err)
	}

	built := 0
	for _, def := range file.Definitions {
		decl, ok := def.(*parser.ConceptDecl)
		if !ok {
			continue
		}
		built++
		if _, err := BuildConceptFromDecl(decl, "v1:ref:"+decl.Name); err != nil {
			t.Errorf("concept %q in dsl/_reference/_concept.memql does not build: %v\n\n"+
				"The reference sheet is what an author copies from, so an example the engine "+
				"refuses teaches a rule that does not exist. Common causes: declaring a "+
				"reserved row intrinsic (id / createdAt / createdBy / concept / type / "+
				"partition / payload / schema -- see section 12) as a payload field, or an "+
				"annotation the parser does not accept.", decl.Name, err)
		}
	}
	if built == 0 {
		t.Fatal("no concepts found in the reference sheet, so this gate measures nothing")
	}
}

// emitSite is the one non-test Go location allowed to spell a declared-metadata
// key: the emitter that puts it into the schema.
const emitSite = "component/database/memory-nodes/concept_parser.go"

// The half that makes the documentation true, and the tripwire on it.
//
// The assertion is not "the key appears once". It is the stronger, and actually
// load-bearing, "the key appears once AND that one appearance is a WRITE" --
// the literal used as the index of an assignment target, `schema[key] = ...`.
// That is what "emitted, read by nothing" means, stated as something a machine
// can check.
//
// It is deliberately an AST scan rather than a grep, because a textual count is
// the wrong instrument for this question and can be defeated three ways, each
// of which was demonstrated against an earlier draft of this test:
//
//   - hoist the key into a named constant and point the emitter at the
//     identifier. The literal count stays at exactly 1 -- it is now the const
//     declaration -- while any number of readers reference the name. Declaring
//     that constant in THIS file also defeats a check that merely pins the
//     path, and this file is precisely where anyone would put it.
//   - build the key by concatenation (`"x-" + "secret"`). No literal matches at
//     all, so a count-based gate reads 0 and reports the emitter missing rather
//     than the reader added.
//   - mention the key in a comment. A grep counts prose as code and fails,
//     telling its author to go and edit three documents that are correct -- and
//     the likeliest author of such a comment is someone documenting the very
//     non-enforcement this test exists to pin.
//
// Walking the AST answers all three: a const declaration is a ValueSpec and not
// an assignment index, so it fails wherever it is written; a concatenation
// yields no matching literal, so the write disappears and the count fails; and
// comments are not AST expressions at all, so they are structurally invisible
// instead of specially-cased.
func TestDeclaredMetadataKeysAreReadByNothing(t *testing.T) {
	root := repoRoot(t)
	for _, key := range []string{"x-unique", "x-immutable", "x-secret"} {
		t.Run(key, func(t *testing.T) {
			const remedy = "\n\nIf enforcement landed, that is good news and three documents are now WRONG. " +
				"Update all of them in the same change:\n" +
				"  - dsl/_reference/_concept.memql section 8 (move it out of DECLARED METADATA, " +
				"and fix the @description on its worked example -- that string ships in the schema)\n" +
				"  - docs/public/language/attribute-matrix.md\n" +
				"  - docs/public/language/reserved.md"

			hits := keyLiteralsInNonTestGo(t, root, key)
			if len(hits) != 1 {
				t.Errorf("%q appears as a literal in %d non-test Go locations, not 1 (the emit site).\n  %s%s",
					key, len(hits), joinHits(hits), remedy)
				return
			}
			hit := hits[0]
			if !hit.isWriteIndex {
				t.Errorf("the sole literal %q at %s is NOT a write (`schema[%q] = ...`).\n\n"+
					"That means the key is no longer merely emitted. The usual cause is hoisting it "+
					"into a named constant -- the literal count stays at 1 while readers reference "+
					"the identifier, which is enforcement.%s",
					key, hit, key, remedy)
				return
			}
			if !strings.HasPrefix(hit.path, emitSite) {
				t.Errorf("the emit of %q moved off %s to %s. If the emitter genuinely moved, update "+
					"emitSite in this test; if a second writer appeared, the docs describe one.%s",
					key, emitSite, hit, remedy)
			}
		})
	}
}

// @default's gap is different in kind: the key IS emitted into the schema, and
// the schema is a *validation* document, so nothing applies it on insert.
// Pinned separately because the fix is separate -- applying it means touching
// the write path, not adding a reader for an x- key.
//
// This is the behaviour CLAUDE.md already states ("a concept-field @default is
// NOT a substitute -- it is never applied on insert either (memql#2960), so ??
// is the only mechanism"), so if it ever changes, CLAUDE.md changes with it.
func TestDefaultIsEmittedButNeverApplied(t *testing.T) {
	props := propertySchemas(t, declaredMetadataFixture)
	if got := props["tier"]["default"]; got != "bronze" {
		t.Fatalf("control broken: @default must reach the schema, got %v", got)
	}
	// A payload omitting the defaulted field validates -- which is the whole
	// point: the schema does not require it and does not fill it. If a future
	// change makes the field required, or makes validation supply the value,
	// this is where it surfaces.
	if _, isRequired := requiredSet(t, declaredMetadataFixture)["tier"]; isRequired {
		t.Error("a @default field became required, which is a behaviour change the reference " +
			"does not describe")
	}

	// The write path itself, because the two assertions above are exactly the
	// schema-inspection failure memql#2960 names ("asserting x-unique is present
	// is what makes this look covered today"). Neither of them changes if
	// Concept.Create starts filling defaults tomorrow, so on their own they are a
	// false PASS on the one behaviour this test is named for.
	assertDefaultNotAppliedOnInsert(t)

	// The parsing half is real but narrower than the reference used to claim:
	// numbers and booleans are coerced, and there is NO datetime branch -- so a
	// datetime field takes a bool default without complaint. Pinned so the
	// corrected sentence in section 8 cannot drift back.
	for _, tc := range []struct {
		name string
		want any
	}{
		{"true", true},
		{"7", int64(7)},
		{"1.5", 1.5},
		{"2026-01-02T03:04:05Z", "2026-01-02T03:04:05Z"}, // NOT parsed as a time
	} {
		if got := parseDefaultValue(tc.name); got != tc.want {
			t.Errorf("parseDefaultValue(%q) = %#v, want %#v -- section 8 describes exactly this "+
				"set, including that RFC3339 is left as a string", tc.name, got, tc.want)
		}
	}
}

// capturingStore is the whole Store surface Concept.Create touches: it keeps
// the node that was actually written so the test can read the payload the
// engine produced rather than the schema it validated against.
type capturingStore struct{ written *MemoryNode }

func (s *capturingStore) InsertMemoryNode(_ context.Context, node *MemoryNode) error {
	s.written = node
	return nil
}

func (s *capturingStore) QueryMemoryNodes(_ context.Context, _ QueryParams) ([]MemoryNode, error) {
	return nil, nil
}

// assertDefaultNotAppliedOnInsert drives the real write path and asserts the
// defaulted field is absent from the stored payload.
//
// This is the assertion the reference sentence actually rests on -- "@default
// ... is NEVER APPLIED on insert -- Concept.Create validates and marshals the
// payload verbatim" -- and the one CLAUDE.md's "?? is the only mechanism that
// fills a value" depends on. Patch Concept.Create to copy schema defaults into
// the payload and this fails; without it, that patch is invisible.
func assertDefaultNotAppliedOnInsert(t *testing.T) {
	t.Helper()

	c := buildFixtureConcept(t, declaredMetadataFixture)
	store := &capturingStore{}
	ctx := provenance.ContextWithProvenance(
		context.Background(),
		provenance.Provenance{Kind: provenance.KindDirect, Name: "declared-metadata-test"},
	)

	// `tier` carries @default("bronze") and is deliberately omitted.
	if _, err := c.Create(ctx, store, CreateParams{
		Actor:   "tester",
		ID:      "probe1",
		Payload: map[string]any{"label": "x"},
	}); err != nil {
		t.Fatalf("control broken: insert omitting the defaulted field must succeed, got %v", err)
	}
	if store.written == nil {
		t.Fatal("control broken: nothing was written, so this measures nothing")
	}

	var stored map[string]any
	if err := json.Unmarshal(store.written.Payload, &stored); err != nil {
		t.Fatalf("unmarshal stored payload: %v", err)
	}
	if got, present := stored["tier"]; present {
		t.Errorf("@default WAS applied on insert: stored payload carries tier=%v.\n\n"+
			"That is enforcement, and it makes two documents WRONG. Update both in the same change:\n"+
			"  - dsl/_reference/_concept.memql section 8 (move @default out of DECLARED METADATA, "+
			"and fix the @description on its three worked examples -- those strings ship in the schema)\n"+
			"  - CLAUDE.md, which states that `??` is the only mechanism that fills a value", got)
	}
}

func requiredSet(t *testing.T, src string) map[string]struct{} {
	t.Helper()
	c := buildFixtureConcept(t, src)
	raw, err := c.DefinitionSchema()
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	var doc struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := map[string]struct{}{}
	for _, r := range doc.Required {
		out[r] = struct{}{}
	}
	return out
}

// repoRoot walks up from this package to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the module root, so this scan measures nothing")
	return ""
}

// keyHit is one string-literal occurrence of a schema key in non-test Go.
type keyHit struct {
	path string // repo-relative, slash-separated
	line int
	// isWriteIndex is true when the literal is the index of an assignment
	// TARGET -- `schema[key] = ...` -- i.e. the emit, as opposed to any read.
	isWriteIndex bool
}

func (h keyHit) String() string { return h.path + ":" + strconv.Itoa(h.line) }

func joinHits(hits []keyHit) string {
	parts := make([]string, 0, len(hits))
	for _, h := range hits {
		s := h.String()
		if h.isWriteIndex {
			s += " (write)"
		} else {
			s += " (READ or declaration)"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n  ")
}

// keyLiteralsInNonTestGo returns every string-literal occurrence of key in a
// non-test .go file under root, each flagged with whether it is the emit write.
//
// Comments are invisible here by construction: they are not expressions, so
// ast.Inspect never yields one. That is the point -- documenting a key is not
// reading it, and a scan that cannot tell those apart punishes the person
// writing the documentation this test exists to protect.
//
// Two limits of scope, stated because the failure messages above assert three
// documents are wrong on the strength of this scan:
//
//   - Go only. A consumer of these keys in sdk/ts or the cockpit would pass
//     unnoticed. The definition schema does reach clients, so the docs' claim is
//     broader than what is mechanically enforced here.
//   - A file that does not parse is skipped rather than failing the run (there
//     is deliberately-invalid Go under testdata trees). A live reader lives in
//     compiling code, so this cannot hide one -- but the count of parsed files
//     is asserted non-zero below, so a scan that silently reached nothing fails
//     instead of passing.
func keyLiteralsInNonTestGo(t *testing.T, root, key string) []keyHit {
	t.Helper()

	quoted := strconv.Quote(key)
	var hits []keyHit
	parsed := 0

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // an unreadable path is not this test's business
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		fset := token.NewFileSet()
		file, parseErr := goparser.ParseFile(fset, path, nil, goparser.SkipObjectResolution)
		if parseErr != nil {
			return nil // not valid Go; cannot contain a live reader
		}
		parsed++

		// First pass: every literal used as the index of an assignment target.
		writes := map[*ast.BasicLit]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range assign.Lhs {
				idx, ok := lhs.(*ast.IndexExpr)
				if !ok {
					continue
				}
				if lit, ok := idx.Index.(*ast.BasicLit); ok && lit.Kind == token.STRING && lit.Value == quoted {
					writes[lit] = true
				}
			}
			return true
		})

		// Second pass: every occurrence of the literal, anywhere.
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || lit.Value != quoted {
				return true
			}
			hits = append(hits, keyHit{
				path:         filepath.ToSlash(rel),
				line:         fset.Position(lit.Pos()).Line,
				isWriteIndex: writes[lit],
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if parsed == 0 {
		t.Fatal("no non-test Go files parsed, so this scan measures nothing")
	}
	return hits
}
