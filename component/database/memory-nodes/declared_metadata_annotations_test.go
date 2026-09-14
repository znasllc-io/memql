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

	"github.com/znasllc-io/memql/core/repowalk"
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
@description("d")
concept probe {
  label      string  @required @description("l")
  uniqueKey  string @description("u")
  externalId string @description("i")
  apiKey     string  @secret @description("s")
  tier       string @description("t")
}
`

// The emission half. An author's declaration must survive into the schema --
// dropping it would make the annotation unrecoverable when enforcement lands,
// and @pii is the precedent for a marker graduating from declared to enforced
// without a schema change.
func TestDeclaredMetadataAnnotationsAreStillEmitted(t *testing.T) {
	props := propertySchemas(t, declaredMetadataFixture)
	// @secret is the ONE survivor of the original four. @unique, @immutable
	// and @default were retired by epic memql#5375, and their keys stopped
	// being emitted with them -- asserted in the other direction by
	// TestRetiredFieldKeywordsAreNoLongerEmitted below, so the pair still
	// covers both states rather than one key quietly leaving the suite.
	for _, tc := range []struct{ field, key string }{
		{"apiKey", "x-secret"},
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
// x-secret LEFT this list in memql#3036. It is now read by
// Concept.SecretFields() through a `json:"x-secret"` struct tag -- the same
// shape as the x-pii reader the positive control below pins -- and drives
// redaction of a rejected value out of validation error messages. Its
// enforcement is PARTIAL by ruling (error messages only; NOT query results,
// NOT structured logs), and the three documents this test used to point at now
// name the covered surface and the uncovered ones explicitly.
//
// It stays pinned by TestSecretEnforcementIsRealAndScoped below, so this is a
// stronger assertion replacing a weaker one rather than a key dropping out of
// the suite.
// After epic memql#5375 this loop is EMPTY, and that is the honest state
// rather than a gap: x-unique and x-immutable were the two keys emitted and
// read by nothing, and both were retired outright. The loop is kept because
// the next marker to arrive in that state belongs here, and because deleting
// the function would take its remedy text -- the three documents a change of
// state has to update -- with it.
func TestDeclaredMetadataKeysAreReadByNothing(t *testing.T) {
	root := repoRoot(t)
	_ = root
	for _, key := range []string{} {
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

// The positive control on the scan itself, and the reason it exists is that the
// gate above can only ever report an ABSENCE. "No reader found" is the same
// output whether there is genuinely no reader or the scan cannot see the kind
// of reader that is there -- so a gate with no positive control is a gate that
// cannot distinguish working from broken.
//
// x-pii is the right control precisely because it is NOT declared metadata: it
// is enforced today, read by Concept.PIIFields through a struct tag. So the
// scan run against it MUST find that reader. If this test starts passing
// vacuously -- or if the tag arm is simplified away -- the three keys above go
// on reporting "read by nothing" whether or not it is still true.
//
// This is the acceptance criterion this PR was parked on, written as a test
// rather than as a claim.
func TestTheScanSeesAStructTagReader(t *testing.T) {
	const (
		enforcedKey = "x-pii"
		readerFile  = "component/database/memory-nodes/concept.go"
	)

	hits := keyLiteralsInNonTestGo(t, repoRoot(t), enforcedKey)

	var tagReader *keyHit
	for i := range hits {
		if !hits[i].isWriteIndex && strings.HasPrefix(hits[i].path, readerFile) {
			tagReader = &hits[i]
			break
		}
	}
	if tagReader == nil {
		t.Fatalf("the scan found no struct-tag reader of %q in %s, but Concept.PIIFields reads it "+
			"there via `json:\"%s\"`.\n  found: %s\n\n"+
			"The scan is therefore blind to the struct-tag form -- which is the idiom this repo "+
			"reads these keys with, and the one memql#3036 will use when it implements @secret "+
			"redaction by copying PIIFields. While that is true, the gate above reports "+
			"\"read by nothing\" for x-unique / x-immutable / x-secret whether or not it is "+
			"still true, and its first real exercise would be the case it cannot see.",
			enforcedKey, readerFile, enforcedKey, joinHits(hits))
	}

	// And the other half: x-pii must NOT pass the "emitted, read by nothing"
	// shape the gate above asserts. If it did, that shape would be satisfiable
	// by an enforced key, and satisfying it would mean nothing.
	if len(hits) == 1 && hits[0].isWriteIndex {
		t.Errorf("%q looks emitted-and-never-read, but it is enforced. The assertion the gate "+
			"above makes is not distinguishing enforced keys from unenforced ones.", enforcedKey)
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
//
// memql#3038 RULED on what to do about that, and the answer was neither "apply
// it" nor "retire it" -- it stays documentation, and the mistake of writing one
// and getting nothing is caught at AUTHORING time instead of never. The gate is
// dsl's TestDefaultIsCoalescedOrStamped, which fails when an optional TOP-LEVEL
// concept field carries @default and no bound mutation stamps it. So this test
// keeps its exact meaning: it pins that the engine does not apply the value,
// which is precisely the premise the gate rests on. If this test ever has to
// change because Create starts filling defaults, that gate becomes wrong in the
// same commit.
//
// The gate's scope is narrower than this test's subject, deliberately, and
// stated so a reader does not infer tree-wide coverage: it scans the IN-REPO
// tree only (a MEMQL_DSL_PATH bundle is never scanned), and it skips NESTED
// object leaves (no write form stamps a single leaf). Nothing is applied at any
// depth, in-repo or mounted -- that part is universal, and it is what this test
// pins.
// TestDefaultOnAConceptFieldIsRetired is what TestDefaultIsEmittedButNeverApplied
// and its three siblings became.
//
// The memql#2960 tripwire at the top of this file said: "When enforcement
// lands for any of them, the second half fails and names the documents that
// have to change in the same commit." Epic memql#5375 fired it in the OTHER
// direction -- not enforcement, retirement. @default on a concept field was
// emitted as the JSON-Schema `default` keyword that no validator applies and
// no insert path consults, and the careful type-directed lowering behind it
// (parseTypedDefaultValue, memql#3248) served only that keyword, so both are
// deleted with the annotation.
//
// @unique and @immutable went the same way. @secret is the one survivor, and
// it survives because it is genuinely ENFORCED -- see
// TestSecretEnforcementIsRealAndScoped below, which is why the set was worth
// separating rather than retiring whole.
func TestDefaultOnAConceptFieldIsRetired(t *testing.T) {
	src := `@version("1.0.0")
@description("d")
concept probe {
  label  string  @required @description("l")
  tier   string  @default("bronze") @description("t")
}
`
	file, err := parser.ParseFile(src)
	if err != nil {
		t.Fatalf("fixture did not parse -- the retirement is a LOAD refusal, not a parse one: %v", err)
	}
	var built bool
	for _, def := range file.Definitions {
		decl, ok := def.(*parser.ConceptDecl)
		if !ok {
			continue
		}
		built = true
		_, err := BuildConceptFromDecl(decl, "v1:ref:probe")
		if err == nil {
			t.Fatal("@default on a concept field should be refused at build")
		}
		if !strings.Contains(err.Error(), "retired") {
			t.Errorf("the refusal should say retired, got: %v", err)
		}
		if !strings.Contains(err.Error(), "memqlmigrate --rewrite=attributes") {
			t.Errorf("the refusal should name the rewrite, got: %v", err)
		}
		if !strings.Contains(err.Error(), "??") {
			t.Errorf("the refusal should name `??` as the mechanism that fills a value, got: %v", err)
		}
	}
	if !built {
		t.Fatal("fixture declared no concept, so it measures nothing")
	}
}

// TestUniqueAndImmutableAreRetired covers the other two of the four. Each was
// declared metadata with nothing behind it -- no uniqueness check (memql#2960)
// and no write guard -- and each was EMITTED as an x- keyword, which is the
// part that made it worse than silence: the schema told the storage layer
// about a constraint no storage layer honoured.
func TestUniqueAndImmutableAreRetired(t *testing.T) {
	for _, field := range []string{"email string @unique", "createdBy string @immutable"} {
		src := "@version(\"1.0.0\")\n@description(\"d\")\nconcept probe {\n  label string @required\n  " + field + "\n}\n"
		file, err := parser.ParseFile(src)
		if err != nil {
			t.Fatalf("%s: fixture did not parse: %v", field, err)
		}
		for _, def := range file.Definitions {
			decl, ok := def.(*parser.ConceptDecl)
			if !ok {
				continue
			}
			if _, err := BuildConceptFromDecl(decl, "v1:ref:probe"); err == nil {
				t.Errorf("%s should be refused at build", field)
			} else if !strings.Contains(err.Error(), "memqlmigrate --rewrite=attributes") {
				t.Errorf("%s: the refusal should name the rewrite, got: %v", field, err)
			}
		}
	}
}

// TestRetiredFieldKeywordsAreNoLongerEmitted is the schema half. The x- keys
// and the `default` key have to STOP being written, or a reader of a generated
// schema still sees a constraint the engine refuses to accept a declaration
// for -- the inverse of the memql#2960 defect and just as misleading.
func TestRetiredFieldKeywordsAreNoLongerEmitted(t *testing.T) {
	props := propertySchemas(t, declaredMetadataFixture)
	for _, key := range []string{"default", "x-unique", "x-immutable"} {
		for field, schema := range props {
			if _, present := schema[key]; present {
				t.Errorf("field %q still emits %q, which memql#5375 retired", field, key)
			}
		}
	}
	// The control: x-secret must still be emitted, because @secret is enforced.
	if _, present := props["apiKey"]["x-secret"]; !present {
		t.Error("control broken: x-secret must still be emitted -- @secret is the one survivor of the four, and it is enforced")
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

// repoRoot walks up from this package to the REPOSITORY root.
//
// It looks for go.work, not go.mod. This scan reads repo-relative paths
// (dsl/_reference/..., docs/...) and reports repo-relative emit sites, and
// since memql#3228 the tree is many modules -- component/database is one of
// them, so a go.mod walk stopped there and the scan looked for
// component/database/dsl/_reference/ and reported emit sites shorn of their
// component/database/ prefix (memql#3242). go.work exists exactly once, at the
// repository root, which is the thing every caller here means.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 12; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the repository root (go.work), so this scan measures nothing")
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
// FOUR limits of scope, listed in full because the failure messages above
// assert three documents are wrong on the strength of this scan, and because
// this list said "two" while a third and fourth existed -- which is the same
// shape of defect the scan is built to catch:
//
//   - Go only. A consumer of these keys in sdk/ts or the cockpit would pass
//     unnoticed. The definition schema does reach clients, so the docs' claim is
//     broader than what is mechanically enforced here.
//   - A file that does not parse is skipped rather than failing the run (there
//     is deliberately-invalid Go under testdata trees). A live reader lives in
//     compiling code, so this cannot hide one -- but the count of parsed files
//     is asserted non-zero below, so a scan that silently reached nothing fails
//     instead of passing.
//   - A READER that builds the key by concatenation (`"x-" + "secret"`) adds no
//     matching literal and is invisible. Note the asymmetry with the emit: a
//     concatenated EMIT is caught, because the literal count drops to zero and
//     the assertion is on an exact count. A concatenated reader simply adds
//     nothing to count.
//   - A reader that never names the key at all -- a generic scan such as
//     `strings.HasPrefix(k, "x-")` over the schema's properties -- is likewise
//     invisible.
//
// The last two are inherent to any key-literal scan and are the price of
// choosing an exact matcher over a heuristic one. They are recorded rather than
// closed; a gate whose limits are written down can be trusted at its edges,
// which is the whole argument this file makes about documentation.
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
			if repowalk.SkipDir(info.Name()) {
				return filepath.SkipDir
			}
			switch info.Name() {
			case ".git", "vendor", "node_modules":
				return filepath.SkipDir
			case ".claude":
				// `.claude/worktrees/` holds local git worktrees -- FULL copies
				// of this repo (see CLAUDE.md). Walking into them counts the
				// same emit site once per worktree, so this test reported "3
				// non-test Go locations" for a key that has exactly one, and
				// did so only on machines where somebody happened to have a
				// worktree checked out. Green in CI, red locally, for a
				// condition that has nothing to do with the code under test.
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

		// Third pass: STRUCT TAGS, which the pass above cannot see and which
		// are the idiom this repo actually reads these keys with.
		//
		// A tag is a *ast.BasicLit too, but its value is `json:"x-pii"` --
		// backquoted, with the key nested inside. It never equals `"x-pii"`,
		// so an equality test skips it entirely. That is not a hypothetical
		// gap: Concept.PIIFields() reads x-pii exactly this way
		// (concept.go:526), and memql#3036 -- approved, and blocked on this
		// PR -- will implement @secret redaction by copying that function.
		// The tripwire's first real exercise was the one case it could not
		// see, which is the worst possible place for a blind spot.
		//
		// Matched by walking *ast.Field.Tag specifically rather than by
		// searching every backquoted string, so an unrelated raw string that
		// happens to contain the key is not counted as a reader.
		ast.Inspect(file, func(n ast.Node) bool {
			field, ok := n.(*ast.Field)
			if !ok || field.Tag == nil {
				return true
			}
			// Both the bare key and the key carrying tag OPTIONS. Matching
			// only `"x-secret"` requires the closing quote immediately after
			// the name, so `json:"x-secret,omitempty"` slips through -- and
			// on a bool field `,omitempty` is the reflexive Go spelling, so
			// that is the MORE likely form of the reader this arm exists to
			// catch, not a contrived one. Missing it would have reproduced
			// the defect that parked this PR, one grammar level down.
			//
			// The trailing quote/comma is what keeps it precise:
			// `json:"x-secret-other"` matches neither and must not fire.
			if !strings.Contains(field.Tag.Value, quoted) &&
				!strings.Contains(field.Tag.Value, `"`+key+`,`) {
				return true
			}
			hits = append(hits, keyHit{
				path: filepath.ToSlash(rel),
				line: fset.Position(field.Tag.Pos()).Line,
				// A tag is a decode instruction: it exists to READ the key
				// out of a document. It is never the emit.
				isWriteIndex: false,
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

// TestSecretEnforcementIsRealAndScoped replaces x-secret's row in the
// read-by-nothing gate above (memql#3036). It asserts the two halves that make
// the documentation true, and it fails from BOTH directions:
//
//   - x-secret must still have a real reader. If SecretFields() is deleted or
//     refactored away, the annotation silently returns to declared metadata
//     while three documents go on claiming it redacts -- the exact
//     over-promise this issue exists to remove.
//   - the documentation must still name EVERY surface and get its state
//     right. Enforcement is only honest if a reader can neither infer blanket
//     redaction from the word "enforced" (query results and length are still
//     uncovered) nor be told a surface leaks when it no longer does. Both
//     directions are checked: a missing surface fails, and so does a stale
//     "not redacted by ..." for one that now redacts.
func TestSecretEnforcementIsRealAndScoped(t *testing.T) {
	root := repoRoot(t)

	t.Run("has a reader", func(t *testing.T) {
		const readerFile = "component/database/memory-nodes/concept.go"
		hits := keyLiteralsInNonTestGo(t, root, "x-secret")

		var reader *keyHit
		for i := range hits {
			if !hits[i].isWriteIndex && strings.HasPrefix(hits[i].path, readerFile) {
				reader = &hits[i]
				break
			}
		}
		if reader == nil {
			t.Fatalf("x-secret has no reader in %s, but Concept.SecretFields must read it there "+
				"via `json:\"x-secret\"`.\n  found: %s\n\n"+
				"If the redaction was deliberately reverted, @secret is declared metadata again "+
				"and THREE documents now over-promise. Put x-secret back in "+
				"TestDeclaredMetadataKeysAreReadByNothing and correct:\n"+
				"  - dsl/_reference/_concept.memql section 8\n"+
				"  - docs/public/language/attribute-matrix.md\n"+
				"  - docs/public/language/reserved.md",
				readerFile, joinHits(hits))
		}
	})

	// Each document must name EVERY unenforced surface. A document that says
	// "enforced" without them lets a reader assume a credential is safe
	// everywhere.
	//
	// The phrases are DISTINCTIVE on purpose. The first version of this gate
	// searched the whole lowercased file for "query result" and "log", and both
	// are satisfied by text that has nothing to do with @secret: "log" is a
	// substring of "logic"/"logical", which all three documents use for
	// unrelated reasons, and attribute-matrix.md documents @cacheSeconds as
	// "Cache query results". Measured: with that gate, deleting the entire
	// @secret caveat from _concept.memql and attribute-matrix.md left it GREEN,
	// and it could not even distinguish these documents from their pre-memql#3036
	// text, which asserted the OPPOSITE ("NOTHING IS REDACTED"). Only
	// reserved.md failed, which is why the original verification -- done on that
	// one file -- read as proof for all three.
	//
	// So each phrase below must be one a reader could only write while
	// describing @secret's scope.
	for _, doc := range []string{
		"dsl/_reference/_concept.memql",
		"docs/public/language/attribute-matrix.md",
		"docs/public/language/reserved.md",
	} {
		t.Run(doc, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(doc)))
			if err != nil {
				t.Fatalf("read %s: %v", doc, err)
			}
			text := strings.ToLower(string(raw))
			if !strings.Contains(text, "@secret") && !strings.Contains(text, "x-secret") {
				t.Fatalf("%s does not mention @secret at all, so it cannot describe its scope", doc)
			}
			// surface -> the alternative spellings that count as naming it,
			// plus (for surfaces that are now ENFORCED) the stale spellings
			// that would mean the document still calls them uncovered.
			//
			// The staleGoneAfterEnforcement half is the half that bites now.
			// Before memql#3182-#3184 these documents had to NAME each
			// uncovered surface so a reader could not infer blanket coverage.
			// Now that four of them redact, the failure mode has INVERTED: a
			// document that still says "not redacted by the tool-args
			// validator" under-promises, and an author reading it will write
			// a workaround for a leak that no longer exists -- or, worse,
			// conclude the annotation is not worth using. Naming alone can no
			// longer distinguish the two states, so the state is asserted.
			for _, surface := range []struct {
				name  string
				anyOf []string
				// staleGoneAfterEnforcement: phrases that must NOT appear,
				// because they assert this surface is still uncovered. Empty
				// for surfaces that genuinely remain uncovered.
				staleGoneAfterEnforcement []string
			}{
				{
					name:  "query results",
					anyOf: []string{"redacted from **query results**", "not redacted from query results", "from **query results**"},
					// Still genuinely uncovered (memql#2803) -- nothing stale.
				},
				{
					name:                      "the automation args binder",
					anyOf:                     []string{"automation args binder", "args_binding.go"},
					staleGoneAfterEnforcement: []string{"not redacted by the automation", "not** redacted by the **automation"},
				},
				{
					name:                      "concept payload validation",
					anyOf:                     []string{"concept payload validation", "by concept payload validation"},
					staleGoneAfterEnforcement: []string{"not redacted by concept payload validation", "not** redacted by **concept payload validation"},
				},
				{
					name:                      "the tool-args validator",
					anyOf:                     []string{"tool-args validator", "validatetoolargs", "tool_execution.go"},
					staleGoneAfterEnforcement: []string{"not redacted by the tool-args validator", "not** redacted by the **tool-args validator"},
				},
				{
					name:  "the by-name matching rule",
					anyOf: []string{"by argument name", "by arg name", "matching is by argument name", "matches by argument name", "name, not by write target", "not by write target"},
				},
				{
					name:  "the length disclosure",
					anyOf: []string{"length is never redacted", "value too long", "rune count for a secret"},
				},
				{
					// Found by the exhaustive enumeration memql#3182's DoD
					// required, and absent from the epic's own four-surface
					// model -- so the documents must name it too, or the next
					// reader re-derives it.
					name:  "the validate/preflight builtins",
					anyOf: []string{"preflight", "executor_builtin.go"},
				},
			} {
				for _, stale := range surface.staleGoneAfterEnforcement {
					if strings.Contains(text, stale) {
						t.Errorf("%s still describes %s as NOT redacted (%q), but it is "+
							"now enforced.\n\nAn under-promising scope document is a real "+
							"failure, not a harmless one: an author reading it writes a "+
							"workaround for a leak that no longer exists, or concludes "+
							"@secret is not worth using. Update the paragraph.",
							doc, surface.name, stale)
					}
				}
				found := false
				for _, phrase := range surface.anyOf {
					if strings.Contains(text, phrase) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("%s must name %s when describing @secret's scope.\n\n"+
						"@secret now redacts on every validation surface that quotes a "+
						"rejected value -- the function-args validator (memql#3036), the "+
						"tool-args validator (#3182), the automation args binder (#3183), "+
						"concept payload validation (#3184), and the validate/preflight "+
						"builtins -- while query results and length remain uncovered, and "+
						"the two args-validator surfaces match by argument NAME rather than "+
						"by write target.\n\n"+
						"Every one of those has to be NAMED, covered or not. The list comes "+
						"from an exhaustive enumeration of validators, never from appending "+
						"one entry at a time: three successive incremental passes each "+
						"shipped a paragraph that walked past a surface the next pass found, "+
						"and the exhaustive sweep turned up two the epic's own four-surface "+
						"model did not contain. A document that says @secret is enforced "+
						"without naming what it does NOT cover recreates the over-promise "+
						"memql#2960 corrected -- one rung higher, and harder to spot now "+
						"that it is mostly true.\n\n"+
						"Accepted spellings: %v", doc, surface.name, surface.anyOf)
				}
			}
		})
	}
}
