package dsl

import (
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslfs"
)

// TestUserDisplayCardStaysMinimal is the guard on the client-callable half of
// the memql#2800 split.
//
// userDisplayById is @public and resolves a CALLER-SUPPLIED id, so it returns
// whatever userDisplayCard projects for an arbitrary user. Today that is id +
// displayName. Every field added there is a PII leak for every user in the
// cluster, and the addition would look completely innocuous in review -- one
// line in a shape, far from the query that makes it dangerous.
//
// This is deliberately a field-set assertion rather than a "no PII" heuristic:
// a denylist of sensitive names would pass anything not on the list, which is
// the wrong default for the one shape that must not grow.
func TestUserDisplayCardStaysMinimal(t *testing.T) {
	src := readTreeFile(t, "identity/shapes.memql")

	start := strings.Index(src, "shape user userDisplayCard {")
	if start < 0 {
		t.Fatal("shape userDisplayCard not found -- if it was renamed, move this guard with it; " +
			"userDisplayById is @public and cross-user, so its projection must stay pinned")
	}
	end := strings.Index(src[start:], "\n}")
	if end < 0 {
		t.Fatal("could not find the end of userDisplayCard")
	}
	body := src[start+len("shape user userDisplayCard {") : start+end]

	got := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "///") {
			continue
		}
		got[line] = true
	}

	want := map[string]bool{"row.id": true, "displayName": true}

	for f := range got {
		if !want[f] {
			t.Errorf("userDisplayCard projects %q, which is not in the pinned set.\n"+
				"This shape backs userDisplayById -- @public, resolved by a caller-supplied\n"+
				"id, so anything here is readable for EVERY user in the cluster. If the\n"+
				"field is genuinely safe to expose that way, add it to `want` in this test\n"+
				"and say why. If it is not, put it on userFull and read it through\n"+
				"userById (owner-or-admin) or currentUser (self) instead.", f)
		}
	}
	for f := range want {
		if !got[f] {
			t.Errorf("userDisplayCard no longer projects %q; the pinned set is stale", f)
		}
	}
}

// serverOnlyRe finds constructs carrying @serverOnly.
//
// `\b`, not `\s*$`: three places match this annotation (here,
// dsl/conformance_test.go's classification gate, and sdk/gen/gen.go's SDK
// skip) and they must agree. They did not -- this one required the annotation
// to be alone on its line, so `@serverOnly // note` exempted the authz gate
// and was skipped by the SDK while being INVISIBLE here, i.e. an exemption
// without the mandatory rationale this test exists to require.
//
// Pinned by TestServerOnlyRegexSeesATrailingComment, because no construct in
// the tree carries a trailing comment today -- so the bug this widening fixes
// is not reproduced by any fixture, and reverting it left the suite green.
var serverOnlyRe = regexp.MustCompile(`(?m)^@serverOnly\b`)

// TestServerOnlyRegexSeesATrailingComment covers the widening directly.
//
// It matters because this regex is one of THREE copies of the same decision
// (here, dsl/conformance_test.go's classification gate, sdk/gen/gen.go's SDK
// skip), and the loader makes a fourth by parsing. When the regex is NARROWER
// than the loader the disagreement is fail-open in the audit: the gate treats
// the construct as server-only and exempts it from per-row-authz
// classification, while Function.ServerOnly is whatever the parser decided.
func TestServerOnlyRegexSeesATrailingComment(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      bool
	}{
		{"alone on its line", "@serverOnly\nquery user x {\n", true},
		{"trailing line comment", "@serverOnly // parked, see #123\nquery user x {\n", true},
		{"trailing block comment", "@serverOnly /* why */\nquery user x {\n", true},
		{"trailing whitespace", "@serverOnly   \nquery user x {\n", true},
		// Must NOT match: a longer annotation that merely starts with the same
		// letters. `\b` is what keeps these apart -- a bare prefix match would
		// read both as the annotation.
		{"longer annotation sharing the prefix", "@serverOnlyish\nquery user x {\n", false},
		// Not line-anchored -> not the annotation. Substring-matching a
		// preamble is how `/// TODO: mark this @serverOnly` cleared a sibling
		// gate with no annotation present.
		{"named inside a doc comment", "/// TODO: mark this @serverOnly\nquery user x {\n", false},
		{"indented", "  @serverOnly\nquery user x {\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := serverOnlyRe.MatchString(tc.src); got != tc.want {
				t.Errorf("serverOnlyRe.MatchString(%q) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

// TestServerOnlyConstructsAreDocumented requires every @serverOnly construct to
// carry a doc comment, and that comment to say WHY caller-scoping is not the
// answer.
//
// The annotation is easy to reach for and easy to misuse: it makes a construct
// unreachable from the wire, which looks like a security improvement even when
// the correct fix was an actor.userId filter. Each construct that carries it
// has a specific reason the obvious fix is wrong -- circular with
// auth, or the actor is not the affected user -- and that reasoning is the part
// a future reader needs. #2800 was parked once precisely because the reasoning
// had not been written down.
func TestServerOnlyConstructsAreDocumented(t *testing.T) {
	paths, err := dslfs.WalkMemqlFiles(Tree())
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}

	found := 0
	for _, p := range paths {
		src := readTreeFile(t, p)
		for _, loc := range serverOnlyRe.FindAllStringIndex(src, -1) {
			found++
			// The doc block is the run of `///` lines immediately above.
			before := src[:loc[0]]
			lines := strings.Split(before, "\n")
			var doc []string
			for i := len(lines) - 1; i >= 0; i-- {
				l := strings.TrimSpace(lines[i])
				if strings.HasPrefix(l, "///") {
					doc = append(doc, l)
					continue
				}
				if l == "" || strings.HasPrefix(l, "@") {
					continue
				}
				break
			}
			blob := strings.ToLower(strings.Join(doc, " "))
			lineNo := strings.Count(before, "\n") + 1

			if len(doc) == 0 {
				t.Errorf("%s:%d  @serverOnly with no doc comment. Say why the construct "+
					"cannot simply be scoped to actor.userId -- that reasoning is the "+
					"whole justification for barring it from the wire instead.", p, lineNo)
				continue
			}
			if !strings.Contains(blob, "actor.userid") {
				t.Errorf("%s:%d  @serverOnly doc does not explain why actor.userId scoping "+
					"is not the fix. Without that, the next reader cannot tell whether this "+
					"is a considered decision or a shortcut around per-row authz.", p, lineNo)
			}
		}
	}

	if found == 0 {
		t.Error("no @serverOnly constructs found. If the last one was removed the annotation " +
			"may now be dead -- check whether the enforcement in " +
			"component/memql/function_validator.go is still exercised by anything.")
	}
	t.Logf("checked %d @serverOnly construct(s)", found)
}
