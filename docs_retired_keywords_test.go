package main

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestDocsDoNotTeachRetiredDeclarationKeywords is the sibling of
// TestDocsDoNotReferencePrefixedConstructNames (#2914 / #2917 / #2979), for the
// half that gate structurally cannot reach.
//
// # Why the name gate cannot catch this
//
// That gate polices prefixed construct NAMES, and it is deliberately narrow: it
// fires only when the tree declares `X` with the kind the prefix asserts. That
// narrowness is what keeps it silent on counter-examples and on bundle-owned
// names, and it is why `mutation space mutationCreateSpace` slipped past --
// `createSpace` is bundle-owned, declared nowhere in this repo, so the name half
// is correctly undecidable.
//
// But the KEYWORD is wrong independently of whether the name resolves. It is a
// syntax claim, not a name claim, and no amount of widening the name gate
// reaches it (memql#2974).
//
// # Why it earns a gate
//
// `mutation` was renamed to `mutate` in memql#2041, and
// component/language/dslspec hard-fails if it is still a construct keyword --
// so a doc teaching it teaches a declaration the parser REJECTS. CLAUDE.md is
// the sharp edge: the drifted line was the canonical "Mutations:" example in its
// Functions section, which is the standing instruction every Claude Code session
// in this repo reads. A worked example there teaching a retired form is
// self-reinforcing -- the instructions produce the drift the gates then reject.
//
// # Declaration position only
//
// The prose word "mutation" is everywhere and legitimate ("the mutation
// writes...", "Mutations:"), so this anchors on the two-identifier-then-brace
// declaration shape at the start of a line rather than on the bare word.
//
// # Scope: markdown, and why not wider
//
// The sibling name gate sweeps every tracked file, and widening this one to
// match was measured rather than assumed. It cannot be, yet: the only
// non-markdown hits of the declaration shape are two fixtures in
// component/memql/callgraph/callgraph_test.go, and they are load-bearing --
// that checker's own regex still matches the retired keyword, so rewriting the
// fixtures turns its tests red. That is memql#3043, filed with the measurement.
// Widening this gate belongs in the same change that fixes the checker, not
// before it; a gate that requires a broken test or a file exemption to go green
// is worse than one with a stated boundary.
//
// # Escape hatch
//
// A doc teaching AGAINST the retired form needs to be able to quote it. Follow
// the name gate's convention: keep the keyword off a declaration-shaped line
// (indent it inside prose, or backtick the keyword separately) rather than
// weakening the pattern.
func TestDocsDoNotTeachRetiredDeclarationKeywords(t *testing.T) {
	// One entry per retired declaration keyword, with what replaced it. The
	// shape is `^<keyword> <ident> <ident> {` -- the two-identifier signature
	// form every construct of this class uses.
	retired := []struct{ keyword, replacement, ref string }{
		{"mutation", "mutate", "memql#2041"},
	}

	out, err := exec.Command("git", "ls-files", "-z", "*.md").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Split(string(out), "\x00")

	var scanned int
	for _, rel := range files {
		if rel == "" {
			continue
		}
		data, readErr := os.ReadFile(rel)
		if readErr != nil {
			// A tracked-but-absent path is somebody else's problem; skipping it
			// silently is what would be wrong, so say so.
			t.Logf("skipping unreadable tracked file %s: %v", rel, readErr)
			continue
		}
		scanned++
		lines := strings.Split(string(data), "\n")
		for _, r := range retired {
			re := regexp.MustCompile(`^[ \t]*` + r.keyword +
				`[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{`)
			for i, line := range lines {
				if !re.MatchString(line) {
					continue
				}
				t.Errorf("%s:%d teaches the retired `%s` declaration keyword -- it was renamed to "+
					"`%s` in %s, and component/language/dslspec hard-fails if it is still a "+
					"construct keyword, so this is a declaration the parser REJECTS.\n"+
					"  %s\n"+
					"Write `%s` instead. If this line is deliberately quoting the old form to "+
					"teach against it, keep the keyword off a declaration-shaped line (indent it "+
					"inside prose, or backtick the keyword separately) rather than weakening this "+
					"pattern.",
					rel, i+1, r.keyword, r.replacement, r.ref, strings.TrimRight(line, "\r"),
					r.replacement)
			}
		}
	}

	// A sweep that silently stops resolving files reports clean forever. The
	// repo has hundreds of tracked markdown files; a handful means the glob or
	// git invocation has broken.
	if scanned < 50 {
		t.Fatalf("only %d markdown files scanned -- the sweep has stopped resolving them and this "+
			"gate would now pass vacuously", scanned)
	}
}

// The gate must actually fire. Without this the regex could be wrong in a way
// that makes it match nothing, and the sweep above would report clean forever --
// the silent-disable shape the sibling gate's own resolver self-check exists to
// catch.
func TestRetiredKeywordGateMatchesADeclarationAndNotProse(t *testing.T) {
	re := regexp.MustCompile(`^[ \t]*mutation[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{`)

	for _, bad := range []string{
		"mutation space mutationCreateSpace {",
		"  mutation participant addAgentToSpace {",
		"\tmutation lead mutationTouchLead {",
	} {
		if !re.MatchString(bad) {
			t.Errorf("the gate does not match a retired declaration, so it would report clean "+
				"forever: %q", bad)
		}
	}

	for _, ok := range []string{
		"mutate space createSpace {",                 // the live form
		"The mutation writes exactly one aggregate.", // prose
		"Mutations:", // a heading
		"| `mutation` | retired, use `mutate` | ",          // a table quoting the keyword
		"a `mutation` declaration used to look like this:", // teaching against it
		"  ev: mutation createSpawnEvent(nodeId: args.id)", // a CALL, not a declaration
		"mutation {", // not the two-identifier shape
	} {
		if re.MatchString(ok) {
			t.Errorf("the gate fires on a legitimate line, which is how a gate gets suppressed "+
				"and then stops catching the real case: %q", ok)
		}
	}
}
