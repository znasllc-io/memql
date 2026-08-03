package auth

import (
	"context"
	"strings"
	"testing"
)

// The actor bootstrap query name is a documented design constraint, and it
// drifted into six places before anyone noticed (memql#2984).
//
// The constraint is real and load-bearing: the read that BUILDS the actor
// cannot itself be filtered on the actor, which is the stated reason the
// `@rowAuthz` grammar has an escape hatch at all. What drifted was the NAME.
// The resolver calls `userByIdSystem`, which is `@serverOnly`. `userById` is
// a different query gated by `requiresOwnerOrAdmin`, so a reader who followed
// the wrong citation landed on an owner-or-admin gate and could only conclude
// the circularity constraint was imaginary.
//
// Correcting six copies does not stop a seventh, so this pins the fact they
// describe.
//
// It asserts on the query the resolver ACTUALLY EXECUTES, through the
// QueryRunner seam, rather than on the text of the source file. The first
// version of this gate grepped the file, and that was defeated in review by
// moving the name into a constant -- the resolver issued the caller-scoped
// query and the gate stayed green. A gate that a rename defeats is worse than
// none, because its failure message promises coverage it does not have.
func TestActorBootstrapExecutesUserByIdSystem(t *testing.T) {
	const canonicalSub = "v1:identity:user:abc123"

	spy := &recordingQueryRunner{}
	r := &IdentityResolver{Engine: spy}

	// The row shape is irrelevant here; nil output is enough to exercise the
	// call. Whatever LoadFromClaims returns, it must have asked for the right
	// query first.
	_, _ = r.LoadFromClaims(context.Background(), map[string]any{"sub": canonicalSub})

	if len(spy.queries) == 0 {
		t.Fatal("LoadFromClaims executed no query at all. If the bootstrap stopped going " +
			"through QueryRunner, this gate no longer covers it and the six places that name " +
			"the bootstrap query in prose are unpinned again (memql#2984).")
	}

	for _, q := range spy.queries {
		if strings.Contains(q, "userByIdSystem") {
			continue
		}
		if strings.Contains(q, "userById") {
			t.Errorf("the actor bootstrap executed %q. It must use userByIdSystem "+
				"(@serverOnly): this is a PRE-ACTOR read, so a caller-scoped filter over the "+
				"actor it is about to build cannot be satisfied (#2800). `userById` is the "+
				"requiresOwnerOrAdmin-gated query and is not the bootstrap.", q)
			continue
		}
		t.Errorf("the actor bootstrap executed an unrecognised query %q. If it was renamed, "+
			"these describe it by name and must move in the same change:\n"+
			"  component/language/parser/rowauthz_binding.go\n"+
			"  component/identity/pat/verifier.go\n"+
			"  dsl/identity/queries.memql\n"+
			"  docs/public/operate/auth/per-row-authz-audit.md\n"+
			"  docs/public/operate/auth/access-model.md\n"+
			"  docs/internal/planning/roadmap.md", q)
	}
}

type recordingQueryRunner struct{ queries []string }

func (s *recordingQueryRunner) ExecuteShaped(_ context.Context, query string) (any, error) {
	s.queries = append(s.queries, query)
	return nil, nil
}
