package memql

// reference_skeletons_test.go gates dsl/_reference/ -- the authoring
// skeletons an author copies from (memql#3660).
//
// The sheet taught `type="references"`, which the engine rejects at load, and
// the retired quoted-canonical target form `target="v1:identity:user"`
// (memql#1067). Copying either produced a boot refusal on the first run.
//
// It rotted because `_reference/` is skipped by every DSL walker
// (core/dslfs/walker.go: the directory is `_`-prefixed AND every file in it
// is), so no loader and no conformance gate could see it. Two tests DO parse
// it -- sense's TestDiagnose_ReferenceFiles_NoErrors and memory-nodes'
// TestReferenceConceptSheetActuallyBuilds -- and both pass on the defective
// sheet, which is the point worth recording:
//
//	PARSING THE SKELETONS DOES NOT CATCH THIS CLASS OF DEFECT.
//
// A relationship's type is a bare string to the parser
// (attributeToRelationshipDecl copies it through) and to BuildConceptFromDecl.
// Only canonicalRelationshipType, reached from the engine's load path,
// rejects it. So the gate that was missing is not a parse -- it is the
// SEMANTIC checks the live tree already gets, applied to the one directory
// those checks structurally cannot reach:
//
//   - relationship types validated against canonicalRelationshipType, which
//     is the same function the load path calls (not a copy of its list);
//   - the retired quoted-target form rejected by the same textual rule
//     test/dslconformance's TestRelationshipTargetsUseImports applies to the
//     live tree;
//   - the `type` glossary in the sheet's own prose validated too, because two
//     of the three occurrences of `references` were in comments -- a gate
//     reading only declarations would have left most of the defect in place.

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/parser"
)

// referenceSkeleton is one parsed authoring skeleton.
type referenceSkeleton struct {
	path   string // repo-relative, for legible failures
	source string
	file   *parser.File // nil when the skeleton declares nothing
}

// loadReferenceSkeletons reads and parses every dsl/_reference/*.memql file.
func loadReferenceSkeletons(t *testing.T) []referenceSkeleton {
	t.Helper()

	dir := filepath.Join(repoRoot(t), "dsl", "_reference")
	paths, err := filepath.Glob(filepath.Join(dir, "*.memql"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	if len(paths) == 0 {
		t.Fatalf("no skeletons found under %s -- this gate is measuring nothing", dir)
	}

	out := make([]referenceSkeleton, 0, len(paths))
	for _, path := range paths {
		rel := filepath.Join("dsl", "_reference", filepath.Base(path))
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Errorf("%s: read: %v", rel, readErr)
			continue
		}
		skeleton := referenceSkeleton{path: rel, source: string(raw)}

		// `logic` and `automation` are lowered by the rewriter rather than
		// dispatched natively, so the rewrite has to run first or those two
		// skeletons fail on their own keyword.
		rewritten, rewriteErr := parser.NormaliseAll(skeleton.source)
		if rewriteErr != nil {
			t.Errorf("%s: rewrite: %v", rel, rewriteErr)
			continue
		}

		parsed, parseErr := parser.ParseFile(rewritten)
		switch {
		case parseErr == nil:
			skeleton.file = parsed
		case errors.Is(parseErr, parser.ErrEmptyInput):
			// A skeleton that is entirely commentary -- _agent.memql is a
			// retirement breadcrumb -- declares nothing. Not an error.
		default:
			t.Errorf("%s: parse: %v", rel, parseErr)
			continue
		}
		out = append(out, skeleton)
	}
	return out
}

// TestReferenceSkeletonsParse is the syntax half: every skeleton must survive
// rewrite plus parse. It duplicates coverage sense already has, deliberately
// -- this file is the one place a reader looks for "what gates _reference/",
// and a syntax failure here explains itself in the same terms as the semantic
// ones below.
func TestReferenceSkeletonsParse(t *testing.T) {
	skeletons := loadReferenceSkeletons(t)
	if len(skeletons) == 0 {
		t.Fatal("no skeletons loaded")
	}
}

// TestReferenceRelationshipsAreValid is the gate that would have caught
// memql#3660. Every @relationship the sheet DECLARES is checked against the
// engine's own load-time rules.
func TestReferenceRelationshipsAreValid(t *testing.T) {
	declarations := 0

	for _, skeleton := range loadReferenceSkeletons(t) {
		if skeleton.file == nil {
			continue
		}
		for _, definition := range skeleton.file.Definitions {
			concept, ok := definition.(*ast.ConceptDecl)
			if !ok {
				continue
			}
			for index, rel := range concept.Relationships {
				if rel == nil {
					continue
				}
				declarations++
				where := skeleton.path + " concept " + concept.Name

				if _, valid := canonicalRelationshipType(rel.Type); !valid {
					t.Errorf("%s relationship[%d]: type %q is not a structural type the "+
						"engine accepts -- an author copying this gets a boot refusal. "+
						"The closed set is canonicalRelationshipType in "+
						"component/memql/relations.go; for a plain foreign key use "+
						"type=\"interactsWith\"", where, index, rel.Type)
				}

				switch strings.ToLower(strings.TrimSpace(rel.Direction)) {
				case relationshipDirectionOutgoing,
					relationshipDirectionIncoming,
					relationshipDirectionBidirectional:
				default:
					t.Errorf("%s relationship[%d]: direction %q is invalid",
						where, index, rel.Direction)
				}

				if strings.TrimSpace(rel.Field) == "" {
					t.Errorf("%s relationship[%d]: field is empty", where, index)
				}
				if strings.TrimSpace(rel.Target) == "" {
					t.Errorf("%s relationship[%d]: target is empty", where, index)
				}
			}
		}
	}

	if declarations == 0 {
		t.Fatal("no @relationship declarations found in dsl/_reference/ -- the sheet " +
			"has stopped demonstrating relationships, so this gate covers nothing")
	}
}

// retiredQuotedTarget matches the retired canonical-ID target form on an
// authored @relationship line. Same textual rule test/dslconformance's
// TestRelationshipTargetsUseImports applies to the live tree, so the sheet is
// held to exactly the standard it is teaching.
var retiredQuotedTarget = regexp.MustCompile(`target\s*=\s*"`)

// TestReferenceRelationshipTargetsUseImports applies memql#1067 to
// dsl/_reference/. The live-tree gate walks via dslfs.WalkMemqlFiles, which
// skips this directory by construction -- so the sheet kept the retired form
// for as long as it did precisely because the rule could not reach it.
func TestReferenceRelationshipTargetsUseImports(t *testing.T) {
	for _, skeleton := range loadReferenceSkeletons(t) {
		for number, line := range strings.Split(skeleton.source, "\n") {
			if strings.HasPrefix(strings.TrimLeft(line, " \t"), "//") {
				continue // prose may quote the retired form to name it
			}
			if !strings.Contains(line, "@relationship") {
				continue
			}
			if retiredQuotedTarget.MatchString(line) {
				t.Errorf("%s:%d uses the retired canonical-ID target form (memql#1067):\n"+
					"  %s\nUse the bare imported concept name instead:\n"+
					"  use identity.concepts.{ user }\n"+
					"  @relationship(type=\"parent\", field=\"ownerUserId\", target=user, "+
					"direction=\"outgoing\")\nSame-namespace targets need no import.",
					skeleton.path, number+1, strings.TrimSpace(line))
			}
		}
	}
}

// glossaryEntry matches one line of the sheet's `type` values glossary:
//
//	//     interactsWith -- the default plain foreign-key edge. ...
//
// The continuation lines of an entry are indented further and carry no `--`,
// so they do not match.
var glossaryEntry = regexp.MustCompile(`^//\s{5}(\w+)\s+--\s`)

// glossaryHeading opens the block glossaryEntry reads.
const glossaryHeading = "`type` values:"

// TestReferenceDocumentedRelationshipTypesAreValid checks the sheet's PROSE.
//
// Two of the three occurrences of `references` in memql#3660 were comments --
// the grammar sketch and this glossary -- so a gate that read only
// declarations would have left most of the defect teaching the wrong thing.
//
// The check is a SUBSET, not an equality: every type the glossary names must
// be one the engine accepts, but the glossary need not name every type the
// engine accepts. That is deliberate. The sheet teaches; it is not a mirror of
// the registry, and it omits `dependsOn` / `formedFrom` on purpose (harness-
// specific, and memql#3655 retires them as structural types). An equality
// check would force this file to move on every registry change, including ones
// an author should not be taught about.
func TestReferenceDocumentedRelationshipTypesAreValid(t *testing.T) {
	documented := 0

	for _, skeleton := range loadReferenceSkeletons(t) {
		inGlossary := false
		for number, line := range strings.Split(skeleton.source, "\n") {
			if strings.Contains(line, glossaryHeading) {
				inGlossary = true
				continue
			}
			if !inGlossary {
				continue
			}
			match := glossaryEntry.FindStringSubmatch(line)
			if match == nil {
				if strings.TrimSpace(line) == "" {
					inGlossary = false // blank line closes the block
				}
				continue
			}
			documented++
			if _, valid := canonicalRelationshipType(match[1]); !valid {
				t.Errorf("%s:%d documents relationship type %q, which the engine "+
					"rejects at load -- an author copying it gets a boot refusal",
					skeleton.path, number+1, match[1])
			}
		}
	}

	if documented == 0 {
		t.Fatalf("found no %q glossary entries in dsl/_reference/ -- either the "+
			"glossary moved or its format changed; re-aim this pin rather than "+
			"deleting it", glossaryHeading)
	}
}
