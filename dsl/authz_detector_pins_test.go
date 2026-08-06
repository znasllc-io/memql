package dsl

import (
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/dslfs"
)

// memql#2799: TestPerRowAuthzClassification is the tree's only gate against a
// user-scoped read shipping without a caller check, and it had drifted out of
// alignment with the language on TWO axes at once -- it matched the
// `payload.`-prefixed field spelling epic #2292 retired, and the `mutation`
// construct keyword memql#2041 renamed to `mutate`. Both drifts were silent:
// the gate reported 0 flagged and that read as "audited and clean".
//
// The lesson is not that either regex was wrong, it is that nothing noticed
// when the language moved out from under them. These pin the two assumptions
// against the corpus, so the next rename fails here instead of quietly
// switching the gate off.

// TestConstructHeaderMatchesTheLanguage: the classifier must see every
// construct kind the corpus actually declares.
func TestConstructHeaderMatchesTheLanguage(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	kinds := map[string]int{}
	for _, p := range paths {
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		for _, m := range constructHeaderRe.FindAllStringSubmatch(string(raw), -1) {
			kinds[m[1]]++
		}
	}
	// Every kind that declares rows must be reached. A zero here means the
	// classifier is walking a subset of the tree and its clean result covers
	// only that subset -- exactly the #2799 failure.
	for _, kind := range []string{"query", "mutate", "seed"} {
		if kinds[kind] == 0 {
			t.Errorf("constructHeaderRe matched no %q declarations; the classifier cannot see them, so its result does not cover them", kind)
		}
	}

	// And the retired keyword must stay gone. If `mutation` ever comes back as
	// a construct keyword, this regex needs updating rather than silently
	// missing every one of them again.
	retired := regexp.MustCompile(`(?m)^[ \t]*mutation[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{`)
	for _, p := range paths {
		f, _ := tree.Open(p)
		raw, _ := io.ReadAll(f)
		f.Close()
		if retired.Match(raw) {
			t.Errorf("%s declares a construct with the retired `mutation` keyword (memql#2041 renamed it to `mutate`); constructHeaderRe would miss it", p)
		}
	}
}

// TestUserScopeFieldReSpelling pins the bare-vs-prefixed distinction that
// #2292 created and that the old detector was on the wrong side of.
func TestUserScopeFieldReSpelling(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		// Bare payload columns -- the post-#2292 spelling, what must match.
		{"ownerUserId==args.x", true},
		{"filter  requestedBy==args.userId", true},
		{"a && createdBy==args.c", true},

		// The RETIRED spelling does NOT match, and that is correct: the `.`
		// makes it a prefixed reference, and TestFilterSyntaxCanonical already
		// bans `payload.` in filter clauses, so it cannot appear in live code.
		// Every remaining occurrence in the tree is prose in a comment.
		{"payload.ownerUserId==args.x", false},

		// Caller, envelope and intrinsic reads -- NOT the row's own column.
		{"actor.userId", false},
		{"id==args.userId", false},
		{"row.createdBy", false},

		// Neighbouring identifiers must not be swallowed.
		{"myOwnerUserId==1", false},
		{"ownerUserIdExtra==1", false},
		{"userIdent==1", false},
	} {
		if got := userScopeFieldRe.MatchString(tc.in); got != tc.want {
			t.Errorf("userScopeFieldRe.MatchString(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestRowSelectionSurface pins what counts as selecting a row: a filter clause
// and an update block's id: selector, and NOT an insert payload -- stamping an
// owner onto a new row is not the same as picking rows by one. Matching insert
// payloads flags 40 create* mutations tree-wide and buries the real finding.
func TestRowSelectionSurface(t *testing.T) {
	insertBody := "  args {\n    ownerUserId  string!\n  }\n  insert {\n    id: args.id\n    ownerUserId: args.ownerUserId\n  }\n"
	if got := rowSelectionSurface(insertBody); strings.Contains(got, "ownerUserId") {
		t.Errorf("insert payload leaked into the selection surface: %q", got)
	}
	filterBody := "  filter  ownerUserId==actor.userId\n  shape   x\n"
	if got := rowSelectionSurface(filterBody); !strings.Contains(got, "ownerUserId") {
		t.Errorf("filter clause missing from the selection surface: %q", got)
	}
	updateBody := "  update {\n    id: args.userId\n    preferences: {\n      k: args.v\n    }\n  }\n"
	if got := rowSelectionSurface(updateBody); !strings.Contains(got, "id: args.userId") {
		t.Errorf("update id: selector missing from the selection surface: %q", got)
	}

	// Comments are excluded, which the old whole-body scan could not manage.
	// Most doc comments in this tree literally spell out
	// "payload.ownerUserId==actor.userId" as prose, so a body-wide match reads
	// documentation as though it were a predicate.
	commentBody := "  // every read is scoped payload.ownerUserId==actor.userId\n  shape x\n"
	if got := rowSelectionSurface(commentBody); strings.TrimSpace(got) != "" {
		t.Errorf("a comment leaked into the selection surface: %q", got)
	}
}
