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
var serverOnlyRe = regexp.MustCompile(`(?m)^@serverOnly\s*$`)

// TestServerOnlyConstructsAreDocumented requires every @serverOnly construct to
// carry a doc comment, and that comment to say WHY caller-scoping is not the
// answer.
//
// The annotation is easy to reach for and easy to misuse: it makes a construct
// unreachable from the wire, which looks like a security improvement even when
// the correct fix was an actor.userId filter. The three constructs that have it
// today each have a specific reason the obvious fix is wrong -- circular with
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
