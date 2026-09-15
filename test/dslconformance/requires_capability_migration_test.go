package dslconformance

// THE NINE MIGRATED CONSTRUCTS, PINNED (epic memql#5166, section E).
//
// Three specs compared the actor's role STRING against one or three literals --
// `requiresAdmin`, `requiresOwnerOrAdmin`, `requiresDeveloperOrAbove` -- and
// nine constructs named one of them. All three are deleted; each construct
// carries an annotation instead.
//
// WHY THIS IS A TABLE AND NOT A DERIVATION. What each construct MEANS is the
// thing that decided its replacement, and meaning is not mechanically
// recoverable from a filter: `sessionsForSubjectAdmin` and `userById` were
// gated identically and diverge here, because one is a credential-adjacent
// admin read and the other is reading a person. So the coupling is DECLARED,
// and the count is asserted below -- a table that silently shrank would be a
// gate measuring less than it claims.
//
// WHAT IT WOULD CATCH. A construct quietly losing its annotation in a later
// edit: the filter still parses, the query still loads, the surface is simply
// open to everybody. That failure is invisible from every other direction --
// there is no error, no empty result, and nothing in the response that says a
// gate used to be there.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
)

type migratedConstruct struct {
	file string
	name string
	// annotation is the exact text the construct must carry, or "" when the
	// migration deliberately left it with NO gate of its own.
	annotation string
	// why records what the construct means, which is what chose its
	// replacement. Printed in the failure so the next reader is not left
	// guessing whether the table or the DSL is wrong.
	why string
}

var migratedConstructs = []migratedConstruct{
	{
		file: "identity/queries.memql", name: "searchUsers",
		annotation: `@requiresRank("developer")`,
		why: "reading the roster is bounded by who works on this cluster, which is a rung " +
			"rather than a permission a role was given",
	},
	{
		file: "identity/queries.memql", name: "pendingUserInvitations",
		annotation: `@requiresRank("developer")`,
		why: "the three roles that can issue an invitation are a contiguous top of the ladder, " +
			"which is what a floor says",
	},
	{
		file: "identity/queries.memql", name: "userById",
		annotation: `@requiresCapability("read", "principal")`,
		why: "reading a person. Developers already read the user list through searchUsers and " +
			"reach the Users app, so a gate naming two slugs was refusing them a row they " +
			"could see in a list",
	},
	{
		file: "identity/queries.memql", name: "sessionsForSubjectAdmin",
		annotation: `@requiresCapability("update", "principal")`,
		why: "credential-adjacent. Developer holds read-on-principal and no update, so this " +
			"preserves the exclusion the deleted spec produced",
	},
	{
		file: "identity/queries.memql", name: "patIdentitiesForUser",
		annotation: `@requiresCapability("update", "principal")`,
		why:        "credential-adjacent, as above",
	},
	{
		file: "identity/queries.memql", name: "nodeTokenIdentitiesAdmin",
		annotation: `@requiresCapability("update", "principal")`,
		why:        "credential-adjacent, as above",
	},
	{
		file: "accounts/queries.memql", name: "invitationsForAccount",
		annotation: `@requiresRank("developer")`,
		why: "it FOLLOWS pendingUserInvitations' gate rather than restating one; the invitation " +
			"concept declares no row tier, so this floor IS the authorization on the read",
	},
	{
		file: "accounts/queries.memql", name: "clientAccountsAll",
		annotation: `@requiresRank("admin")`,
		why: "the owner-check conjunct is DROPPED and the concept's tier decides. The floor " +
			"predates this epic and is unchanged",
	},
	{
		file: "accounts/queries.memql", name: "clientAccountById",
		annotation: `@requiresRank("admin")`,
		why:        "as clientAccountsAll",
	},
}

// retiredSpecs are the three names no .memql file may name as a CONJUNCT again.
var retiredSpecs = []string{"requiresAdmin", "requiresOwnerOrAdmin", "requiresDeveloperOrAbove"}

func TestMigratedConstructsCarryTheirAnnotation(t *testing.T) {
	for _, m := range migratedConstructs {
		src := readDslFile(t, m.file)
		head := headBefore(t, src, m.name, m.file)
		if !strings.Contains(head, m.annotation) {
			t.Errorf("%s in %s does not carry %s.\n  it means: %s\n\n"+
				"A construct that loses its gate still parses, still loads, and is simply open "+
				"to everybody -- there is no error and nothing in the response that says a gate "+
				"used to be there.",
				m.name, m.file, m.annotation, m.why)
		}
	}
}

// TestNoConstructNamesARetiredSpec is the other half: the annotation being
// present says nothing if the conjunct came back beside it.
func TestNoConstructNamesARetiredSpec(t *testing.T) {
	root := dslRoot(t)
	scanned := 0
	var offenders []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		// THE SHARED SKIP LIST. `.claude/` holds git WORKTREES -- whole copies
		// of this repo -- so a walk that descends into one reads a colleague's
		// branch and fails on code that is not in this tree (memql#4871,
		// memql#4878).
		if info.IsDir() {
			if repowalk.SkipDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".memql") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		scanned++
		rel := strings.TrimPrefix(filepath.ToSlash(path), filepath.ToSlash(root)+"/")
		src := stripMemqlComments(string(body))
		for _, spec := range retiredSpecs {
			if regexp.MustCompile(`\b` + spec + `\b`).MatchString(src) {
				offenders = append(offenders, rel+": "+spec)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	// THE REACHABLE POSITIVE. Without it a broken walk or a changed extension
	// would leave this gate passing while reading nothing -- and "nothing names
	// the retired specs" is exactly the answer an empty scan gives.
	if scanned < 100 {
		t.Fatalf("scanned only %d .memql files under %s; the walk is not reaching the tree, "+
			"so this gate is passing while measuring nothing", scanned, root)
	}
	if len(offenders) > 0 {
		t.Fatalf("these files name a spec epic memql#5166 deleted:\n  %s\n\n"+
			"A role comparison is not a spec any more. `@requiresRank(\"x\")` is a FLOOR on the "+
			"cluster's ladder and `@requiresCapability(\"verb\", \"resource\")` is a GRANT; both "+
			"are validated at load and both see a custom role, which a slug comparison never "+
			"could.", strings.Join(offenders, "\n  "))
	}
}

// TestTheMigrationTableCoversEveryUse asserts the table has not shrunk. Nine
// constructs named one of the three specs when the epic began; a table with
// fewer rows measures a subset and reports success for the rest.
func TestTheMigrationTableCoversEveryUse(t *testing.T) {
	const expected = 9
	if len(migratedConstructs) != expected {
		t.Fatalf("the migration table names %d constructs; epic memql#5166 migrated %d.\n"+
			"If a construct was legitimately retired, remove its row AND say so here -- a "+
			"shrinking table is a gate measuring less than it claims.",
			len(migratedConstructs), expected)
	}
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func dslRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../../dsl")
	if err != nil {
		t.Fatalf("resolving the dsl root: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("cannot reach %s: %v -- if the tree moved, update this path; a gate pointing "+
			"at nothing checks nothing", root, err)
	}
	return root
}

func readDslFile(t *testing.T, rel string) string {
	t.Helper()
	path := filepath.Join(dslRoot(t), rel)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v -- if the file moved, update the table", path, err)
	}
	return string(body)
}

// headBefore returns the text between the end of the previous construct and
// this construct's signature -- the annotation block, and nothing from the
// construct above it. The bound is constructHead's, shared with
// carriesAnnotationGate so the two cannot disagree about where a head starts.
func headBefore(t *testing.T, src, name, file string) string {
	t.Helper()
	sig := regexp.MustCompile(`(?m)^(?:query|mutation|logic)\s+\w+\s+` + regexp.QuoteMeta(name) + `\s*\{`)
	loc := sig.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("no construct named %q in %s -- if it was renamed, update the migration table; "+
			"a table pointing at nothing checks nothing", name, file)
	}
	return constructHead(src, loc[0])
}

// stripMemqlComments blanks `//` and `///` lines so a spec DISCUSSED in prose
// -- and these files discuss the deleted three at length, deliberately, as
// history -- is not read as a live conjunct.
func stripMemqlComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			b.WriteByte('\n')
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
