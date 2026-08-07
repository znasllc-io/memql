package memql

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// Row-authz ENFORCEMENT on the WRITE path (memql#3174, Phase 5 of the
// #2803 ruling) -- the counterpart to memql#3172's read path.
//
// 120 of 215 mutations in the tree take a caller-supplied target id with
// nothing relating it to actor.userId, and the mutation grammar cannot
// express such a relation: zero mutations carry a filter, and `update`
// takes `id:` plus field assignments and nothing else. So the guard
// cannot live in the DSL -- the engine resolves the target row and
// refuses the write when the row's declared owner is not the actor.
//
// The fixtures below use REAL declared concepts (v1:notes:note for the
// owned tier, v1:telephony:call for clusterOwner) rather than
// hand-built declarations, so a change to what the loader produces
// shows up here rather than being papered over.
//
// A note on the shape of these tests: they call the guard directly with
// the PRIOR row's payload, which is exactly what executeWrite hands it
// -- before the read-merge overwrites that payload with the caller's
// delta. The end-to-end claim (the write actually does not land) is
// postgres-gated and lives in rowauthz_write_guard_db_test.go.

// writeGuardOwnedConcept declares `@rowAuthz(owner="ownerUserId")`.
// Same fixture the read-path tests use, deliberately: one declaration
// governs both directions (epic decision B), so both sides must be
// demonstrable against the same concept.
const writeGuardOwnedConcept = declaredOwnedConcept

// writeGuardClusterOwnerConcept declares `@rowAuthz(clusterOwner)`.
const writeGuardClusterOwnerConcept = declaredClusterOwnerConcept

// adminCtx is an `admin` caller who is NOT the cluster owner.
//
// Constraint 2 of memql#3174 in test form: `admin` is not an escape
// without a decision, and no such decision was taken. auth's own
// IsClusterOwner() is `Role == RoleOwner` alone, so this context must
// be refused exactly like any other stranger.
func adminRoleCtx(userId string) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: userId,
		Role:   auth.Role("admin"),
	})
}

// anonymousCtx carries no AccessContext at all.
func anonymousCtx() context.Context { return context.Background() }

// ownedRow is a stored payload for the owned-tier fixture.
func ownedRow(t *testing.T, owner string) map[string]any {
	t.Helper()
	decl := declFor(t, writeGuardOwnedConcept)
	if decl.Tier != langparser.RowAuthzOwned {
		t.Fatalf("%s declares %q; this fixture needs the owned tier",
			writeGuardOwnedConcept, decl.Tier)
	}
	return map[string]any{decl.Owner: owner, "title": "a row"}
}

const (
	victimRowId = writeGuardOwnedConcept + ":victim-shortid"
	// The update contract: a prior row exists and is required.
	priorExists  = true
	priorMissing = false
	isUpdate     = true
	isInsert     = false
)

// AC 1 (the headline): a caller updating another user's row is refused.
//
// This is memql#2991 parts 2 and 3 in miniature. `updateCalendarEvent`
// splats the caller's payload onto a caller-named row AND re-stamps
// `ownerUserId: actor.userId`, so before this guard the write did not
// merely edit a stranger's row, it REASSIGNED it. `deleteCalendarEvent`
// is the same targeting hole with a `deleted: true` field map.
func TestWriteGuardRefusesUpdatingAnotherUsersRow(t *testing.T) {
	err := guardRowAuthzWrite(callerCtx("user-attacker"), writeGuardOwnedConcept,
		victimRowId, ownedRow(t, "user-victim"), priorExists, isUpdate)
	if err == nil {
		decl := declFor(t, writeGuardOwnedConcept)
		t.Fatalf("user-attacker was allowed to write a row owned by user-victim on %s, "+
			"which declares %q. The mutation grammar cannot express the owner predicate "+
			"(zero mutations in the tree carry a filter), so if the engine does not refuse "+
			"here nothing does -- and the mutations on this concept re-stamp ownerUserId "+
			"from the actor, so the write is a takeover, not just an edit (memql#3174).",
			writeGuardOwnedConcept, InjectedPredicate(decl))
	}
}

// AC 1, the other half: the legitimate owner is unaffected.
//
// Without this the guard could be a blanket refusal and the test above
// would still be green.
func TestWriteGuardLeavesTheLegitimateOwnerAlone(t *testing.T) {
	if err := guardRowAuthzWrite(callerCtx("user-owner"), writeGuardOwnedConcept,
		victimRowId, ownedRow(t, "user-owner"), priorExists, isUpdate); err != nil {
		t.Fatalf("the row's own owner was refused their own write: %v", err)
	}
}

// AC: `clusterOwner` behaves as declared -- on BOTH tiers.
//
//   - owned tier: the cluster owner is an ESCAPE (constraint 2). The
//     row is not theirs and they write it anyway.
//   - clusterOwner tier: the cluster owner is the DECLARED audience, so
//     the same caller is admitted through the tier rather than through
//     the escape, and everybody else is refused.
func TestWriteGuardClusterOwnerBehavesAsDeclared(t *testing.T) {
	t.Run("owned tier: cluster owner escapes", func(t *testing.T) {
		if err := guardRowAuthzWrite(ownerRoleCtx("user-clusterowner"), writeGuardOwnedConcept,
			victimRowId, ownedRow(t, "user-victim"), priorExists, isUpdate); err != nil {
			t.Fatalf("the cluster owner was refused: %v", err)
		}
	})

	t.Run("clusterOwner tier: cluster owner writes", func(t *testing.T) {
		decl := declFor(t, writeGuardClusterOwnerConcept)
		if decl.Tier != langparser.RowAuthzClusterOwner {
			t.Fatalf("%s declares %q; this fixture needs the clusterOwner tier",
				writeGuardClusterOwnerConcept, decl.Tier)
		}
		if err := guardRowAuthzWrite(ownerRoleCtx("user-clusterowner"), writeGuardClusterOwnerConcept,
			writeGuardClusterOwnerConcept+":a-call", map[string]any{"disposition": "completed"},
			priorExists, isUpdate); err != nil {
			t.Fatalf("the cluster owner was refused a write to a clusterOwner-tier row: %v", err)
		}
	})

	t.Run("clusterOwner tier: a writer is refused", func(t *testing.T) {
		if err := guardRowAuthzWrite(callerCtx("user-stranger"), writeGuardClusterOwnerConcept,
			writeGuardClusterOwnerConcept+":a-call", map[string]any{"disposition": "completed"},
			priorExists, isUpdate); err == nil {
			t.Fatalf("a plain writer edited a row on %s, which declares the clusterOwner tier",
				writeGuardClusterOwnerConcept)
		}
	})
}

// CONSTRAINT 2, stated as a test: `admin` is NOT an escape.
//
// Deliberately not inferred from the read side. auth's own owner bit is
// `Role == RoleOwner` and nothing widens it, so an admin editing a row
// they do not own is a stranger's write.
func TestWriteGuardDoesNotTreatAdminAsAnEscape(t *testing.T) {
	if err := guardRowAuthzWrite(adminRoleCtx("user-admin"), writeGuardOwnedConcept,
		victimRowId, ownedRow(t, "user-victim"), priorExists, isUpdate); err == nil {
		t.Fatal("an `admin` caller wrote a row owned by someone else. admin is not a " +
			"declared escape on the write path and no decision widened it to one " +
			"(memql#3174 constraint 2); only clusterOwner and a per-write internal-origin " +
			"stamp bypass this guard.")
	}
}

// AC / CONSTRAINT 3: a missing target row does not authorize.
//
// memql#2982's analyzer had to be fixed once for exactly this shape
// (`e486d0f5`, "the analyzer failed OPEN on lowered AST nodes"). On the
// update contract there IS no row to resolve an owner from, so the only
// two honest answers are refuse and report-not-found; falling through
// to the write is neither.
func TestWriteGuardRefusesAMissingTargetRow(t *testing.T) {
	if err := guardRowAuthzWrite(callerCtx("user-anyone"), writeGuardOwnedConcept,
		victimRowId, nil, priorMissing, isUpdate); err == nil {
		t.Fatal("an update naming a row that does not exist was authorized. The guard has " +
			"no owner to compare against, so this must refuse or report not-found -- " +
			"never fall through (memql#3174 constraint 3).")
	}
}

// FINDING 4 of memql#3172, on the write side: no caller identity, no
// write. An empty actor.userId compared against an empty stored owner
// MATCHES, which would hand an unauthenticated caller every orphaned
// row on the concept.
func TestWriteGuardRefusesWhenTheCallerHasNoIdentity(t *testing.T) {
	decl := declFor(t, writeGuardOwnedConcept)
	orphan := map[string]any{decl.Owner: ""}
	if err := guardRowAuthzWrite(anonymousCtx(), writeGuardOwnedConcept,
		victimRowId, orphan, priorExists, isUpdate); err == nil {
		t.Fatal("a caller with no identity wrote a row whose owner is the empty string. " +
			"An `ownerUserId == ''` comparison is a legal predicate that MATCHES; the " +
			"read path refuses this and the write path must agree (memql#3172 finding 4).")
	}
}

// A row that cannot say who owns it is not a row anyone owns.
func TestWriteGuardRefusesARowThatCannotSayWhoOwnsIt(t *testing.T) {
	decl := declFor(t, writeGuardOwnedConcept)
	cases := map[string]map[string]any{
		"owner field absent": {"title": "no owner here"},
		"owner not a string": {decl.Owner: 42},
		"empty payload":      {},
	}
	for name, prior := range cases {
		t.Run(name, func(t *testing.T) {
			if err := guardRowAuthzWrite(callerCtx("user-anyone"), writeGuardOwnedConcept,
				victimRowId, prior, priorExists, isUpdate); err == nil {
				t.Fatalf("a write landed on a row whose declared owner field (%q) is unreadable; "+
					"fail closed", decl.Owner)
			}
		})
	}
}

// AC: the escape hatches are enumerated in ONE place, and this is the
// enumeration read back.
//
// rowAuthzWriteEscape is the only function that answers "does this
// caller bypass the guard", so the whole policy is one switch a reviewer
// can read top to bottom. This test pins the answer for every caller
// shape the request path produces, so widening the set is a visible edit
// here rather than a line somewhere in the guard body.
func TestTheWriteGuardEscapeSetIsEnumeratedInOnePlace(t *testing.T) {
	cases := []struct {
		name    string
		ctx     context.Context
		escapes bool
		reason  string
	}{
		{"no identity at all", anonymousCtx(), false, ""},
		{"authenticated writer", callerCtx("user-a"), false, ""},
		{"admin (NOT an escape -- constraint 2)", adminRoleCtx("user-admin"), false, ""},
		{"cluster owner", ownerRoleCtx("user-operator"), true, "cluster owner"},
		{"internal origin", auth.ContextWithInternalOrigin(callerCtx("user-a")), true, "internal origin"},
		// Trust is about the CHANNEL, not the identity: the identity
		// resolver stamps internal origin before an actor exists.
		{"internal origin, no identity", auth.ContextWithInternalOrigin(anonymousCtx()), true, "internal origin"},
		// The wire says so explicitly, and it must not be undone.
		{"explicit client origin", auth.ContextWithClientOrigin(callerCtx("user-a")), false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, escaped := rowAuthzWriteEscape(tc.ctx)
			if escaped != tc.escapes {
				t.Fatalf("escape=%t, want %t (reason %q). The escape set is exactly "+
					"{internal origin, cluster owner}; changing it is an authorization "+
					"decision, not an implementation detail (memql#3174).", escaped, tc.escapes, reason)
			}
			if tc.escapes && !strings.Contains(reason, tc.reason) {
				t.Fatalf("escaped for reason %q, want one mentioning %q", reason, tc.reason)
			}
		})
	}
}

// AC / CONSTRAINT 1: the internal-origin escape is scoped PER WRITE, and
// a stamped escape does not leak to the next construct in the same
// request.
//
// memql#2989 built and REFUTED the alternative -- stamping internal
// origin onto a request-derived context, which opens every @serverOnly
// construct (and now this guard) for the rest of that request. The shape
// that landed instead is #3072's: workertoken.Store.ListForUser stamps
// onto a SEPARATE context used for one query and nothing else.
//
// Both halves are asserted. The first is the property. The second is the
// control that proves the first is not vacuous: if the stamp were bound
// to a variable that flows on, the second write WOULD be admitted -- so
// this test can tell the two shapes apart, which is the only way it is
// evidence of anything.
func TestInternalOriginEscapeDoesNotLeakToTheNextWriteInTheSameRequest(t *testing.T) {
	requestCtx := callerCtx("user-attacker")
	victim := ownedRow(t, "user-victim")

	// The trusted server-side write: stamped INLINE, as the argument to
	// the one call that needs it.
	if err := guardRowAuthzWrite(auth.ContextWithInternalOrigin(requestCtx), writeGuardOwnedConcept,
		victimRowId, victim, priorExists, isUpdate); err != nil {
		t.Fatalf("a write stamped with internal origin was refused: %v -- the escape has to "+
			"exist or every server-side maintenance write on a declared concept breaks", err)
	}

	// THE PROPERTY: the next construct in the same request is back to
	// client origin, because the stamp died with the call above.
	if err := guardRowAuthzWrite(requestCtx, writeGuardOwnedConcept,
		victimRowId, victim, priorExists, isUpdate); err == nil {
		t.Fatal("the internal-origin escape LEAKED: a second write on the request's own " +
			"context was admitted after an earlier write stamped the escape. That is the " +
			"request-scoped shape memql#2989 refuted; the stamp must be inline on a " +
			"context the trusted caller constructs (memql#3174 constraint 1).")
	}

	// THE CONTROL: the leaky shape, so the assertion above is known to
	// be capable of failing. Binding the stamp to a variable that flows
	// on is what made memql#2879's hole reachable.
	leaked := auth.ContextWithInternalOrigin(requestCtx)
	if err := guardRowAuthzWrite(leaked, writeGuardOwnedConcept,
		victimRowId, victim, priorExists, isUpdate); err != nil {
		t.Fatalf("the control did not behave as a leak (%v), so the property assertion "+
			"above proves nothing", err)
	}
}

// SCOPE BOUNDARY: a create has no target row, so this guard has nothing
// to resolve and does not engage. The insert-side hole -- a raw
// `insert(` forging the owner field on a create -- is memql#3059 /
// task #3175, and stating it here keeps someone from reading this
// file's name as a claim it covers writes in general.
func TestWriteGuardLeavesCreatesAlone(t *testing.T) {
	if err := guardRowAuthzWrite(callerCtx("user-anyone"), writeGuardOwnedConcept,
		writeGuardOwnedConcept+":brand-new", nil, priorMissing, isInsert); err != nil {
		t.Fatalf("a create on a declared concept was refused (%v). A create has no target "+
			"row to own; the owner-forging hole on that path is memql#3059 / task #3175.", err)
	}
}

// COVERAGE BOUNDARY: an undeclared concept is untouched. 13 of 101
// concepts declare a tier today, and widening that population is task
// #3173's job, not this guard's.
func TestWriteGuardLeavesUndeclaredConceptsAlone(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	// Derived rather than named: 88 of 101 concepts declare nothing, and
	// pinning one by name means this test starts measuring the wrong
	// thing the moment task #3173's ratchet moves it.
	undeclared := ""
	for _, c := range memoryNodes.List() {
		if c != nil && c.RowAuthz == nil {
			undeclared = c.Name
			break
		}
	}
	if undeclared == "" {
		t.Skip("every loaded concept declares a tier; there is no undeclared boundary left")
	}
	if err := guardRowAuthzWrite(callerCtx("user-anyone"), undeclared,
		undeclared+":someone-elses", map[string]any{"ownerUserId": "user-victim"},
		priorExists, isUpdate); err != nil {
		t.Fatalf("a write to an UNDECLARED concept was refused (%v). The guard reaches "+
			"exactly the concepts that declare a tier; the rest is task #3173.", err)
	}
}

// The declared owner is read from the PRIOR row, never from the delta.
//
// This is the difference between a guard and a formality. Every
// owned-tier mutation in the tree re-stamps `ownerUserId: actor.userId`
// (that is what makes them pass the read-side land gate), so a guard
// that consulted the merged payload would compare the attacker against
// the attacker and always agree.
func TestWriteGuardReadsTheOwnerFromThePriorRowNotTheDelta(t *testing.T) {
	decl := declFor(t, writeGuardOwnedConcept)
	// The prior row belongs to the victim. The delta claims otherwise --
	// but executeWrite calls the guard BEFORE the merge, so the guard
	// only ever sees this map.
	prior := map[string]any{decl.Owner: "user-victim", "title": "theirs"}
	if err := guardRowAuthzWrite(callerCtx("user-attacker"), writeGuardOwnedConcept,
		victimRowId, prior, priorExists, isUpdate); err == nil {
		t.Fatal("the guard admitted a write whose prior row is owned by someone else. If " +
			"this ever passes, check that the call site still runs BEFORE " +
			"mergePayloadFields -- that function overwrites the prior payload in place, " +
			"and every mutation on this concept re-stamps the owner from the actor.")
	}
}

// THE LAND-TIME GATE for the write side (memql#3174), the counterpart to
// TestRowAuthzEnforcementLandGate.
//
// It states day-one coverage rather than implying it: which concepts
// declare a tier, and which mutation constructs bind them and therefore
// change behaviour on merge. A silent drop in that number means a
// declaration was removed and the guard quietly stopped reaching those
// writes.
//
// It also fails on a `granted` declaration. The write path has no filter
// to have performed the relationship join a granted tier needs, so the
// guard REFUSES that tier rather than guessing -- which is the right
// fail-closed answer and also a behaviour nobody should discover from
// production. Zero concepts declare granted today; the day one does,
// this test says so first.
func TestWriteGuardDayOneCoverage(t *testing.T) {
	registry := loadedTreeRegistry(t)

	declared := map[string]string{} // concept -> tier
	for _, c := range memoryNodes.List() {
		if c == nil || c.RowAuthz == nil {
			continue
		}
		declared[c.Name] = string(c.RowAuthz.Tier)
	}

	guarded := map[string][]string{} // concept -> mutations
	mutations := 0
	for _, fn := range registry.List() {
		if fn == nil || fn.FunctionKind != "mutation" {
			continue
		}
		mutations++
		bound := strings.TrimSpace(fn.BoundConcept)
		if _, ok := declared[bound]; !ok {
			continue
		}
		guarded[bound] = append(guarded[bound], fn.Name)
	}

	concepts := make([]string, 0, len(declared))
	for name := range declared {
		concepts = append(concepts, name)
	}
	sort.Strings(concepts)

	var b strings.Builder
	b.WriteString("\n=== rowAuthz write guard, day-one coverage (memql#3174) ===\n\n")
	for _, name := range concepts {
		sort.Strings(guarded[name])
		b.WriteString(name + "  [" + declared[name] + "]  " +
			strings.Join(guarded[name], ", ") + "\n")
	}
	b.WriteString("\nconcepts declaring a tier   " + strconv.Itoa(len(declared)) + "\n")
	b.WriteString("mutation constructs         " + strconv.Itoa(mutations) + "\n")
	t.Log(b.String())

	if len(declared) == 0 {
		t.Fatal("no concept declares a tier -- the guard reaches nothing and this gate " +
			"would pass forever")
	}
	for _, name := range concepts {
		if declared[name] == string(langparser.RowAuthzGranted) {
			t.Errorf("%s declares the `granted` tier. The write guard cannot decide that "+
				"tier against a single row -- its predicate is a relationship spec and a "+
				"write has no filter to have performed the join -- so it REFUSES every "+
				"non-escape write to this concept. Adjudicate before merging: either the "+
				"concept takes a decidable tier, or the guard grows a way to evaluate a "+
				"spec against one row (memql#3174).", name)
		}
	}
}

