package dslconformance

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/dslfs"
	"github.com/znasllc-io/memql/dsl"
)

// default_stamped_test.go -- memql#3038, SUPERSEDED BY epic memql#5375 and cut
// down to the one assertion that still has a subject.
//
// # What this file used to be
//
// A concept-field `@default("value")` reached the emitted schema as the JSON
// Schema `default` keyword, which was ANNOTATION, NOT BEHAVIOUR: validators do
// not fill it and Concept.Create marshals the payload verbatim, so an omitted
// field was simply absent. The failure memql#3038 closed was not the missing
// value -- it was that THE AUTHOR NEVER FOUND OUT. Writing `@default("draft")`
// looked like it did something and nothing said otherwise, so this file grew
// ~500 lines that walked the tree, paired every `@default` field against the
// mutations writing its concept, and demanded a `??` coalesce or an explicit
// stamp for each one.
//
// # Why almost none of it survives
//
// memql#5375 retired the annotation. A concept field carrying `@default` now
// REFUSES AT LOAD, naming `??` as the mechanism and `memqlmigrate
// --rewrite=attributes` as the fix -- which is the author finding out, at the
// first moment they could, from the engine rather than from a conformance run.
// The elaborate pairing was an approximation of a gate the language can now
// make directly, and an approximation kept alongside the real thing is a second
// rulebook that drifts.
//
// What is left is the corpus half: the tree must carry none. That is worth
// keeping separately from the load refusal because the refusal only fires on
// what a node actually loads, and this walks every file including the ones a
// given build does not mount.
//
// `@default` on a TOOL, PROMPT or BUILTIN field is untouched and deliberately
// not scanned here: those bodies ARE the JSON schema handed to the model, so
// `default` there is a value the model reads rather than a keyword nothing
// applies.

// dsConceptFieldDefault matches a `@default(...)` on a field line. Anchored on
// leading whitespace because a concept's fields are indented and its own
// header annotations are not -- and `@default` on a PROVIDER is a header
// annotation that survives.
var dsConceptFieldDefault = regexp.MustCompile(`(?m)^[ \t]+.*@default\(`)

// TestNoConceptFieldDefaultInTheTree is what memql#3038's ~500 lines reduce to
// once the annotation itself is refused.
func TestNoConceptFieldDefaultInTheTree(t *testing.T) {
	tree := dsl.Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}

	var hits []string
	scanned := 0
	for _, p := range paths {
		if !strings.HasSuffix(p, "concepts.memql") {
			continue
		}
		scanned++
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if dsConceptFieldDefault.MatchString(line) {
				hits = append(hits, fmt.Sprintf("%s:%d", p, i+1))
			}
		}
	}

	// The meaningless-zero guard this file's own history earned: a detector
	// that walks nothing reports a clean tree, which is how the memql#3043
	// lesson reads in one line.
	if scanned == 0 {
		t.Fatal("scanned 0 concepts.memql files -- the walk is broken, so a clean result covers nothing")
	}
	if len(hits) > 0 {
		t.Errorf("%d concept-field @default annotation(s) in the tree; the annotation is RETIRED "+
			"(epic memql#5375) and refuses at load, so this is a tree that will not boot. Fill the "+
			"value with `??` in the mutation that writes it, or run `memqlmigrate --rewrite=attributes`:\n  %s",
			len(hits), strings.Join(hits, "\n  "))
	}
}
