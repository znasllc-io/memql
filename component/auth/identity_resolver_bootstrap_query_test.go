package auth

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The actor bootstrap query name is a documented design constraint, and it
// drifted into SIX documents before anyone noticed (memql#2984).
//
// The claim is: the read that BUILDS the actor cannot itself be filtered on
// the actor, so the one construct creating the actor needs an escape hatch
// from caller-scoping. That constraint is real and load-bearing -- it is the
// stated reason the `@rowAuthz` grammar has an escape hatch at all.
//
// What drifted was the NAME. The resolver calls `userByIdSystem`, which is
// `@serverOnly`. `userById` is a different query gated by
// `requiresOwnerOrAdmin`. Documents naming `userById` sent readers to an
// owner-or-admin-gated query, from which the honest conclusion is that the
// circularity constraint is imaginary. It reached
// component/language/parser/rowauthz_binding.go, dsl/identity/queries.memql,
// docs/public/operate/auth/{per-row-authz-audit,access-model}.md,
// docs/internal/planning/roadmap.md and component/identity/pat/verifier.go.
//
// Correcting six copies does not stop a seventh. This pins the FACT the
// copies describe: if the bootstrap query is ever renamed, this fails and
// names the documents that must move with it. That is the cheapest available
// gate -- the prose itself cannot be checked mechanically, but the thing the
// prose is about can.
func TestActorBootstrapQueryIsUserByIdSystem(t *testing.T) {
	const path = "identity_resolver.go"

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(data)

	if !strings.Contains(src, "userByIdSystem") {
		t.Fatalf("%s no longer mentions userByIdSystem. If the actor bootstrap query was "+
			"renamed, these all describe it by name and must be updated in the same change:\n"+
			"  component/language/parser/rowauthz_binding.go\n"+
			"  component/identity/pat/verifier.go\n"+
			"  dsl/identity/queries.memql\n"+
			"  docs/public/operate/auth/per-row-authz-audit.md\n"+
			"  docs/public/operate/auth/access-model.md\n"+
			"  docs/internal/planning/roadmap.md", path)
	}

	// The bootstrap must not be spelled as the caller-scoped query. Matches
	// `query userById(` but not `query userByIdSystem(` -- \b does not fire
	// between "d" and "S", both word characters.
	callerScoped := regexp.MustCompile(`query\s+userById\b`)
	if loc := callerScoped.FindStringIndex(src); loc != nil {
		line := 1 + strings.Count(src[:loc[0]], "\n")
		t.Errorf("%s:%d issues `query userById`, the requiresOwnerOrAdmin-gated query. The "+
			"actor bootstrap must use userByIdSystem (@serverOnly): it is a PRE-ACTOR read, so "+
			"a caller-scoped filter over the actor it is about to build cannot be satisfied "+
			"(#2800).", path, line)
	}
}
