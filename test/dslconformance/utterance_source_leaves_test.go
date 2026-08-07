package dslconformance

import (
	"bufio"
	"github.com/znasllc-io/memql/dsl"
	"io"
	"strings"
	"testing"
)

// memql#2794: three queries filtered on `v1:cognition:utterance.source` leaves
// the concept did not declare. All three ARE written -- Go stamps agentId, kind
// and feedbackAnnouncePlanId onto the source map -- so the schema was what was
// wrong, not the reads. This pins the two back together.
//
// What this enforces is a CONVENTION, not a schema-enforced closure. Nested
// blocks are open at runtime: concept_parser.go emits
// `additionalProperties: false` for the top-level object only (the one nested
// emission of it covers @variant arms), so an undeclared sub-field inserts and
// reads back fine. That is precisely why the drift went unnoticed -- nothing
// rejected it at either end. The value of declaring is that the schema
// describes the row, so the next author filtering on `source.` can see what is
// there instead of guessing.
//
// Deliberately scoped to this one block rather than validating every nested
// path in the tree. General leaf validation has to distinguish blocks that
// enumerate their sub-fields from free-form `object` fields (planner's
// feedbackRequest.timeoutAt is undeclared by design) and collapse `@variant`
// arms (identity's credentials.keyHash lives in an arm, not on the parent),
// which is its own change -- see the issue. `source` enumerates its
// sub-fields, so it is checkable today.
const checkedInCognitionConcepts = "cognition/concepts.memql"

// declaredSourceLeaves reads the sub-field names of the `source { ... }` block
// on the utterance concept.
func declaredSourceLeaves(t *testing.T) map[string]bool {
	t.Helper()
	f, err := dsl.Tree().Open(checkedInCognitionConcepts)
	if err != nil {
		t.Fatalf("open %s: %v", checkedInCognitionConcepts, err)
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read %s: %v", checkedInCognitionConcepts, err)
	}

	leaves := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	// Scoped to the utterance concept by brace DEPTH, not by the first bare
	// `}`: utterance declares other nested blocks (timestamps, action), so a
	// naive close-brace match ends the concept early. `source {` happens to be
	// unambiguous across the tree today, but a second one declared above would
	// silently redirect an unscoped scan, and a guard whose whole job is to not
	// be silently wrong should not rely on that.
	depth := 0
	inUtterance := false
	inSource := false
	sawSource := false
	for sc.Scan() {
		trimmed := strings.TrimSpace(sc.Text())
		opens := strings.Count(trimmed, "{")
		closes := strings.Count(trimmed, "}")

		switch {
		case !inUtterance:
			if strings.HasPrefix(trimmed, "concept utterance ") || trimmed == "concept utterance {" {
				inUtterance = true
				depth = opens - closes
			}
			continue
		case inSource:
			if closes > 0 && opens == 0 {
				inSource = false
			} else if fields := strings.Fields(trimmed); len(fields) > 0 {
				// `  name  type  @description(...)` -- first token is the field.
				leaves[fields[0]] = true
			}
		case trimmed == "source {":
			inSource, sawSource = true, true
		}

		depth += opens - closes
		if depth <= 0 {
			break // End of concept utterance.
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", checkedInCognitionConcepts, err)
	}
	if !sawSource {
		t.Fatalf("concept utterance in %s declares no `source {` block; the concept shape changed and this guard has silently stopped protecting anything", checkedInCognitionConcepts)
	}
	if len(leaves) == 0 {
		t.Fatalf("found no sub-fields in the utterance `source` block of %s; the block shape changed and this guard has silently stopped protecting anything", checkedInCognitionConcepts)
	}
	return leaves
}

// TestUtteranceSourceLeavesAreDeclared: every `source.<leaf>` a filter reads
// must be a declared sub-field of the closed `source` block.
func TestUtteranceSourceLeavesAreDeclared(t *testing.T) {
	declared := declaredSourceLeaves(t)

	type violation struct {
		file string
		line int
		leaf string
		pred string
	}
	var violations []violation

	visitFilterPredicates(t, func(file string, lineno int, pred string) {
		head, rest := splitFilterRef(pred)
		if head != "source" || !strings.HasPrefix(rest, ".") {
			return
		}
		leaf := rest[1:]
		// Stop at the first non-identifier byte: `.agentId==args.x` -> agentId.
		for i := 0; i < len(leaf); i++ {
			c := leaf[i]
			if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
				leaf = leaf[:i]
				break
			}
		}
		if leaf == "" || declared[leaf] {
			return
		}
		violations = append(violations, violation{file, lineno, leaf, pred})
	})

	for _, v := range violations {
		t.Errorf("%s:%d filters on source.%s, which the utterance `source` block does not declare. Nested blocks are open at runtime, so this reads back whatever is stored rather than failing -- which is the problem: either add the sub-field to the concept (if a writer stamps it) or fix the path (if none does):\n  %s",
			v.file, v.line, v.leaf, v.pred)
	}
}
