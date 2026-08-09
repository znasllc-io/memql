package memql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// rowauthz_pii_unbound_test.go -- memql#3350.
//
// The hole: `browseConceptPage("v1:identity:user")`, the generic keyset
// browse any client may issue, returned every user row -- eight @pii
// fields included -- to any authenticated caller whatever their role,
// because the concept declares no `@rowAuthz` tier and an undeclared
// concept admitted unconditionally.
//
// The issue's second acceptance box asks for exactly one thing:
// "A test asserting a `reader` cannot read another user's row through
// the GENERIC browse, not only through `searchUsers`." That is
// TestReaderCannotBrowseAnotherUsersRow. The rest of this file exists so
// that test cannot pass for the wrong reason -- a browse that is
// accidentally bound, a gate that also breaks the named reads, or a
// cache that serves an admin's page to a reader.

const piiConcept = "v1:identity:user"

// loadConcepts makes the unified concept registry available. Every test
// here resolves @pii and @rowAuthz off the LOADED concept rather than a
// hand-built fixture, so a change to the .memql source moves the tests.
func loadConcepts(t *testing.T) {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
}

// userRow builds a stored v1:identity:user row for the row gate.
func userRow(t *testing.T, id string) (string, []byte) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"displayName":  "Alice Example",
		"primaryEmail": "alice@example.com",
		"role":         "reader",
		"active":       true,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return id, payload
}

// unboundCtx is a read that resolved NO declared concept binding -- what
// the engine stamps for a raw client-supplied query string.
func unboundCtx(ctx context.Context) context.Context {
	return contextWithRowAuthzBinding(ctx, "")
}

// boundCtx is a read through a named construct bound to conceptName.
func boundCtx(ctx context.Context, conceptName string) context.Context {
	return contextWithRowAuthzBinding(ctx, conceptName)
}

func piiActorCtx(userId string, role auth.Role) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: userId,
		Role:   role,
	})
}

// THE ACCEPTANCE TEST (memql#3350 box 2).
//
// A reader, through the GENERIC browse, must not receive another user's
// row. Asserted against rowAuthzAdmits -- the row gate the unbound path
// actually runs -- rather than against a hand-rolled predicate.
func TestReaderCannotBrowseAnotherUsersRow(t *testing.T) {
	loadConcepts(t)
	id, payload := userRow(t, "v1:identity:user:alice")

	ctx := unboundCtx(piiActorCtx("bob", auth.RoleReader))

	if got := rowAuthzAdmits(ctx, piiConcept, id, payload); got != rowAuthzDeny {
		t.Fatalf(`a reader was admitted another user's %s row through the GENERIC browse.

  row:      %s
  caller:   bob (role=reader)
  verdict:  %v, wanted rowAuthzDeny

browseConceptPage sends a RAW query string, so it carries no declared
binding, filter injection resolves nothing for it, and the row gate is
the ONLY enforcement it gets. The named surface is gated --
searchUsers and userById both carry requiresOwnerOrAdmin -- but the
generic browse does not go through them, and it returns rawNodes():
the full payload, including all eight @pii fields.`, piiConcept, id, got)
	}
}

// The subject still reads their OWN row. A gate that denied this would
// break /me and every self-service surface.
func TestSubjectCanBrowseTheirOwnRow(t *testing.T) {
	loadConcepts(t)
	id, payload := userRow(t, "v1:identity:user:alice")

	ctx := unboundCtx(piiActorCtx("alice", auth.RoleReader))

	if got := rowAuthzAdmits(ctx, piiConcept, id, payload); got != rowAuthzAdmit {
		t.Fatalf("the subject was refused their own %s row: %v, wanted rowAuthzAdmit", piiConcept, got)
	}
}

// The subject match survives the canonical-vs-bare id split.
//
// memql#3172 caught this the hard way: an owner field is an outgoing
// @relationship, so the stored spelling is canonical
// (`v1:identity:user:alice`) while the actor envelope carries the BARE
// `alice`. A raw `==` is false for every row. This gate delegates to
// sameRowAuthzOwner for that reason; the test pins the delegation.
func TestSubjectMatchNormalizesIdSpelling(t *testing.T) {
	loadConcepts(t)
	_, payload := userRow(t, "")

	for _, tc := range []struct{ name, rowId, caller string }{
		{"canonical row, bare caller", "v1:identity:user:alice", "alice"},
		{"canonical row, canonical caller", "v1:identity:user:alice", "v1:identity:user:alice"},
		{"bare row, bare caller", "alice", "alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := unboundCtx(piiActorCtx(tc.caller, auth.RoleReader))
			if got := rowAuthzAdmits(ctx, piiConcept, tc.rowId, payload); got != rowAuthzAdmit {
				t.Fatalf("subject refused their own row (row=%q caller=%q): %v",
					tc.rowId, tc.caller, got)
			}
		})
	}
}

// Owner and admin keep the cross-user read. This is the audience
// searchUsers already admits (`requiresOwnerOrAdmin`), and the portal
// People view (#3322) plus the VS Code concept browser (#3301) depend on
// it. A gate that denied them would have replaced a leak with an outage.
func TestOwnerAndAdminCanBrowseAnyUserRow(t *testing.T) {
	loadConcepts(t)
	id, payload := userRow(t, "v1:identity:user:alice")

	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleAdmin} {
		t.Run(string(role), func(t *testing.T) {
			ctx := unboundCtx(piiActorCtx("operator", role))
			if got := rowAuthzAdmits(ctx, piiConcept, id, payload); got != rowAuthzAdmit {
				t.Fatalf("role %q was refused another user's row on the generic browse: %v.\n"+
					"This is the audience requiresOwnerOrAdmin admits on searchUsers; the two "+
					"must not disagree about who may see the full user row.", role, got)
			}
		})
	}
}

// The roles BELOW the owner/admin pair are all denied, not just reader.
// `developer` is the interesting one: it is engineering power, ranks
// alongside admin in RoleLevel, and is still not on the PII read.
func TestNonAdminRolesAreAllDeniedAnotherUsersRow(t *testing.T) {
	loadConcepts(t)
	id, payload := userRow(t, "v1:identity:user:alice")

	for _, role := range []auth.Role{auth.RoleDeveloper, auth.RoleWriter, auth.RoleReader} {
		t.Run(string(role), func(t *testing.T) {
			ctx := unboundCtx(piiActorCtx("bob", role))
			if got := rowAuthzAdmits(ctx, piiConcept, id, payload); got != rowAuthzDeny {
				t.Fatalf("role %q was admitted another user's row on the generic browse: %v, "+
					"wanted rowAuthzDeny", role, got)
			}
		})
	}
}

// An unauthenticated caller gets nothing. The row gate's ambient rule
// (memql#2801: a missing AccessContext DENIES) has to hold here too --
// an anonymous browse is the worst case, not an exempt one.
func TestAnonymousCallerIsDeniedPIIRows(t *testing.T) {
	loadConcepts(t)
	id, payload := userRow(t, "v1:identity:user:alice")

	ctx := unboundCtx(context.Background())
	if got := rowAuthzAdmits(ctx, piiConcept, id, payload); got != rowAuthzDeny {
		t.Fatalf("an anonymous caller was admitted a %s row on the generic browse: %v", piiConcept, got)
	}
}

// A BOUND read is untouched.
//
// This is the property that lets the gate exist at all. userDisplayById
// is @public and usersActiveInSpace is ungated -- they render one
// participant's name in another's chat, and they are how the product
// works. They go through a named construct, so they carry a binding, and
// this gate must have no opinion about them.
func TestBoundReadsAreUnaffected(t *testing.T) {
	loadConcepts(t)
	id, payload := userRow(t, "v1:identity:user:alice")

	ctx := boundCtx(piiActorCtx("bob", auth.RoleReader), piiConcept)
	if got := rowAuthzAdmits(ctx, piiConcept, id, payload); got != rowAuthzAdmit {
		t.Fatalf(`a BOUND read of another user's row was denied: %v.

The gate is scoped to unbound (raw-query) reads on purpose. A bound read
went through a named construct with a filter and a shape chosen by an
author; re-deciding it here would break userDisplayById (@public) and
usersActiveInSpace, and would be a second authorization opinion
competing with the first.`, got)
	}
}

// An UNSTAMPED context is not treated as unbound.
//
// Nothing stamps outside the engine's plan seam, so an unstamped context
// is a read that never came through it -- graph expansion re-entering the
// gate, or an in-process caller. Treating those as unbound would apply a
// rule about the client browse to reads that are not it.
func TestUnstampedContextIsNotTreatedAsUnbound(t *testing.T) {
	loadConcepts(t)
	id, payload := userRow(t, "v1:identity:user:alice")

	ctx := piiActorCtx("bob", auth.RoleReader) // deliberately not stamped
	if got := rowAuthzAdmits(ctx, piiConcept, id, payload); got != rowAuthzAdmit {
		t.Fatalf("an unstamped context was treated as unbound: %v", got)
	}
}

// Trusted server-side Go passes, mirroring clause 1 of
// rowAuthzWriteEscape. The identity service, the seed materializer and
// the admin handlers all read user rows this way, and every one of them
// stamps internal origin from an allow-listed package.
func TestInternalOriginIsNotGated(t *testing.T) {
	loadConcepts(t)
	id, payload := userRow(t, "v1:identity:user:alice")

	ctx := unboundCtx(auth.ContextWithInternalOrigin(piiActorCtx("bob", auth.RoleReader)))
	if got := rowAuthzAdmits(ctx, piiConcept, id, payload); got != rowAuthzAdmit {
		t.Fatalf("internal origin was gated on the unbound read path: %v", got)
	}
}

// A concept with NO @pii fields is untouched on the unbound path.
// The gate is keyed off the annotation, not off a concept allow-list, so
// browsing an ordinary concept must behave exactly as before.
func TestNonPIIConceptIsUnaffectedOnUnboundReads(t *testing.T) {
	loadConcepts(t)

	const plain = "v1:cluster:node"
	c, err := memorynodes.Get(plain)
	if err != nil || c == nil {
		t.Skipf("fixture concept %s not loaded", plain)
	}
	if len(c.PIIFields()) != 0 {
		t.Fatalf("fixture %s now declares @pii fields (%v) -- pick a different plain concept",
			plain, c.PIIFields())
	}

	ctx := unboundCtx(piiActorCtx("bob", auth.RoleReader))
	if got := rowAuthzAdmits(ctx, plain, plain+":n1", []byte(`{"name":"bff"}`)); got != rowAuthzAdmit {
		t.Fatalf("a non-PII concept was gated on the unbound read path: %v", got)
	}
}

// A DECLARED tier wins outright; this gate never fires for one.
//
// A declaration is a deliberate authorization statement. If a concept
// ever declares BOTH a tier and @pii fields, the tier decides -- one
// authorization opinion per concept.
func TestDeclaredTierTakesPrecedence(t *testing.T) {
	loadConcepts(t)

	// v1:notes:note declares @rowAuthz(owner="ownerUserId") and no @pii.
	// The owner tier must still be what answers, on the unbound path too.
	payload := []byte(`{"ownerUserId":"alice"}`)
	ctx := unboundCtx(piiActorCtx("alice", auth.RoleReader))
	if got := rowAuthzAdmits(ctx, declaredOwnedConcept, declaredOwnedConcept+":n1", payload); got != rowAuthzAdmit {
		t.Fatalf("the declared owner tier did not admit the owner on an unbound read: %v", got)
	}
	ctxOther := unboundCtx(piiActorCtx("bob", auth.RoleReader))
	if got := rowAuthzAdmits(ctxOther, declaredOwnedConcept, declaredOwnedConcept+":n1", payload); got != rowAuthzDeny {
		t.Fatalf("the declared owner tier did not deny a non-owner on an unbound read: %v", got)
	}
}

// THE POPULATION, pinned (memql#3350 box 3).
//
// The issue's third box asks that "the same question" be answered for
// the other @pii-bearing concepts. The measured answer is that THERE ARE
// NONE: v1:identity:user is the only concept in the tree declaring any
// @pii field. That is a fact about the tree, not a claim, so it is
// asserted rather than written down -- and because the gate is keyed off
// the annotation, a concept that grows a @pii field is covered the
// moment it does.
//
// This test fails when that population changes, which is the signal to
// re-ask the question for the newcomer rather than to edit the constant.
func TestPIIBearingConceptPopulation(t *testing.T) {
	loadConcepts(t)

	want := map[string]bool{piiConcept: true}

	got := map[string][]string{}
	for name, c := range memorynodes.All() {
		if c == nil {
			continue
		}
		if fields := c.PIIFields(); len(fields) > 0 {
			got[name] = fields
		}
	}

	for id, fields := range got {
		if !want[id] {
			t.Errorf(`%s now declares @pii fields %v and is NOT accounted for by memql#3350.

Answer the same question for it, then add it here:
  - Does it declare an @rowAuthz tier? If so the tier decides and this
    gate never fires for it -- that is a complete answer.
  - If not, it is now DENIED to non-subject, non-admin callers on the
    generic browse by rowauthz_pii_unbound.go. Confirm that is right for
    this concept: the subject key is the ROW ID, so a concept whose
    subject is a payload field (ownerUserId, userId) will deny everyone
    but an admin. That is fail-closed and safe, but if the concept needs
    finer access the fix is to declare a tier, not to widen this gate.`,
			id, fields)
		}
	}
	for id := range want {
		if _, ok := got[id]; !ok {
			t.Errorf("%s no longer declares any @pii field. If its PII was removed, drop it "+
				"from this test AND revisit the note on the concept in dsl/identity/concepts.memql, "+
				"which records why the concept carries no @rowAuthz tier.", id)
		}
	}
}

// The eight @pii fields memql#3350 names are the ones actually declared.
// If the set shrinks silently, the issue's description stops matching the
// tree and the next reader cannot tell what was protected.
func TestUserPIIFieldsAreDeclared(t *testing.T) {
	loadConcepts(t)

	c, err := memorynodes.Get(piiConcept)
	if err != nil || c == nil {
		t.Fatalf("concept %q is not loaded: %v", piiConcept, err)
	}
	have := map[string]bool{}
	for _, f := range c.PIIFields() {
		have[f] = true
	}
	// The four memql#3350 calls out by name, which is the set the issue's
	// severity argument rests on.
	for _, want := range []string{"displayName", "firstName", "lastName", "primaryEmail"} {
		if !have[want] {
			t.Errorf("%s.%s is no longer @pii; memql#3350 names it as part of the exposure",
				piiConcept, want)
		}
	}
}

// The concept still declares NO tier.
//
// If someone declares one, this gate stops firing for v1:identity:user
// (a declared tier wins) and the browse hole would silently reopen unless
// the tier genuinely covers it. Fail here so that is a decision, not a
// side effect.
func TestUserConceptStillDeclaresNoTier(t *testing.T) {
	loadConcepts(t)

	c, err := memorynodes.Get(piiConcept)
	if err != nil || c == nil {
		t.Fatalf("concept %q is not loaded: %v", piiConcept, err)
	}
	if c.RowAuthz != nil {
		t.Fatalf(`%s now declares @rowAuthz(%v).

That is a real improvement, but it CHANGES WHAT GUARDS THE GENERIC
BROWSE: rowAuthzAdmits consults the declared tier first, so
rowauthz_pii_unbound.go no longer fires for this concept. Confirm the
new tier denies a reader another user's row on an UNBOUND read, then
retire this test along with the @pii gate's dependence on the concept.

Read the note above `+"`concept user`"+` in dsl/identity/concepts.memql first --
it records the three measured reasons a tier was not declared
(cross-user @public reads, pre-actor reads, the admin roll-up).`, piiConcept, c.RowAuthz)
	}
}

// The generic browse really is UNBOUND.
//
// Everything above rests on it. If browseConceptPage's query string ever
// resolved a declared binding, the gate would silently stop applying and
// every test here would still pass. Parse the exact string the SDK sends
// and assert the plan carries no binding.
func TestGenericBrowseOverUserIsStamped(t *testing.T) {
	loadConcepts(t)

	e := &MemQLEngine{initialized: true, functions: newFunctionRegistry()}

	// Verbatim from sdk/ts/src/client/conceptBrowser.ts.
	const browse = `sort(paginate(concept==v1:identity:user, 200), "createdAt", "asc")`

	plan, err := e.parseWithFunctions(browse, e.functions, nil, false)
	if err != nil {
		t.Fatalf("parse the generic browse string: %v", err)
	}
	if strings.TrimSpace(plan.BoundConcept) != "" {
		t.Fatalf(`the generic browse now resolves a declared binding (%q).

If that is deliberate, this gate no longer applies to it -- filter
injection does, and enforceRowAuthzOnPlan would AND the concept's tier
into the browse instead. Re-derive which mechanism guards the browse
before assuming it is still guarded.`, plan.BoundConcept)
	}
	if !planIsUnbound(plan) {
		t.Fatal("planIsUnbound disagrees with plan.BoundConcept; the cache key and the row gate " +
			"would then disagree about which reads are unbound")
	}
}

// The cache cannot serve one caller's browse page to another.
//
// The row gate drops rows per caller, so an unbound read is now
// actor-dependent. Neither pre-existing cache-key condition catches it:
// an unbound plan never sets RowAuthzInjected, and the raw browse string
// references no actor. Without the boundness term an admin's full user
// list is cached and handed to the next reader -- the hole this change
// closes, reopened one layer up.
func TestUnboundPlansAreActorKeyedInTheCache(t *testing.T) {
	loadConcepts(t)

	e := &MemQLEngine{initialized: true, functions: newFunctionRegistry()}
	const browse = `sort(paginate(concept==v1:identity:user, 200), "createdAt", "asc")`

	plan, err := e.parseWithFunctions(browse, e.functions, nil, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	admin := e.planCacheSignature(piiActorCtx("operator", auth.RoleAdmin), plan)
	reader := e.planCacheSignature(piiActorCtx("bob", auth.RoleReader), plan)

	if admin == reader {
		t.Fatalf(`the generic browse produces ONE cache signature for an admin and a reader:

  %q

The row gate drops rows per caller, so the cached result is the FIRST
caller's view. An admin browsing v1:identity:user would populate the
entry and a reader would then be served every user row from cache,
bypassing the gate entirely.`, admin)
	}
	if !strings.HasPrefix(admin, "actor:") {
		t.Fatalf("unbound plan signature is not actor-keyed: %q", admin)
	}
}
