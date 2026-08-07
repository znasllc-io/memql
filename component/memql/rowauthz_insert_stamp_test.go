package memql

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/provenance"
)

// Row-authz OWNER STAMPING on the raw `insert(` path (memql#3175,
// carrying memql#3059).
//
// The two sibling files answer the other two questions. #3172's read
// path decides who may SEE a row; #3174's write guard decides who may
// write an EXISTING row. Neither reaches a CREATE over the raw surface:
// a create has no target row to resolve an owner from, so its problem is
// STAMPING rather than guarding, and #3174 says so in
// TestWriteGuardLeavesCreatesAlone. This file is that gap closed.
//
// Shape of these tests, deliberately the same as #3174's: they call the
// stamp directly, with fixtures taken from REAL declared concepts rather
// than hand-built declarations, so a change in what the loader produces
// shows up here instead of being papered over. The end-to-end claim --
// the row that LANDS carries the caller as its owner, driven through
// handleExecuteQuery -- is postgres-gated and lives in
// component/grpc/rowauthz_insert_stamp_db_test.go.
//
// The one runnable wiring proof is
// TestExecuteWriteConsultsTheOwnerStamp: it drives the real executeWrite
// and asserts the refusal comes from this stamp rather than from the
// store. It fails against the pre-change tree, where the same call
// reached storage with the payload as supplied.

// stampProbeEngine boots a DB-free engine carrying the full concept
// registry. executeWrite's own DB reads are skipped for an id-less
// create, so this is enough to drive the real write path up to the
// store -- which is where a DB-free engine stops.
func stampProbeEngine(t *testing.T) *MemQLEngine {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := New(nil)
	if err != nil {
		t.Fatalf("New(nil): %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatalf("engine.Init: %v", err)
	}
	return eng
}

// The two values of MutationNode.FromTemplate, named at the call site so
// a reader does not have to decode a bare boolean.
const (
	rawWrite       = false
	templatedWrite = true
)

// rawInsert is a MutationNode as parseInsertFunction produces one: no
// FromTemplate, because nothing rendered a template. The zero value is
// the untrusted case on purpose -- see the field's doc comment.
func rawInsert(conceptName string, payload map[string]any) MutationNode {
	raw, _ := json.Marshal(payload)
	return MutationNode{Concept: conceptName, PayloadRaw: string(raw)}
}

// rawInsertCtx is what engine.go stamps onto a raw insert() before
// dispatch: an actor plus `provenance.Direct("rawInsert:<concept>")`.
// Without the provenance the row layer refuses every write, so a test
// omitting it would be measuring the wrong refusal.
func rawInsertCtx(ctx context.Context, conceptName, actor string) context.Context {
	ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: actor})
	return provenance.ContextWithProvenance(ctx, provenance.Direct("rawInsert:"+conceptName))
}

// forgedOwnerPayload is the attack in one line: a create on a declared
// concept naming somebody else as the owner. `body` is present because
// v1:notes:note requires it -- the payload has to be otherwise VALID or
// the write stops at schema validation and proves nothing about
// ownership.
func forgedOwnerPayload(t *testing.T, victim string) map[string]any {
	t.Helper()
	decl := declFor(t, declaredOwnedConcept)
	return map[string]any{decl.Owner: victim, "title": "forged", "body": "taken"}
}

// AC 1 (the headline), at the seam: a caller-supplied value for the
// declared owner field is OVERWRITTEN by the actor's id, not merged
// with it and not left alone.
func TestRawInsertOverwritesACallerSuppliedOwner(t *testing.T) {
	decl := declFor(t, declaredOwnedConcept)
	payload := forgedOwnerPayload(t, "user-victim")

	if err := stampRowAuthzOwner(callerCtx("user-attacker"), declaredOwnedConcept, payload, rawWrite); err != nil {
		t.Fatalf("the stamp refused an ordinary authenticated create: %v", err)
	}
	if got := payload[decl.Owner]; got != "user-attacker" {
		t.Fatalf("%s = %v after the stamp, want %q. A caller-supplied owner must be "+
			"OVERWRITTEN: the raw insert( surface bypasses the mutation template, so "+
			"`stamp { %s: actor.userId }` never ran and nothing else was going to set it "+
			"(memql#3059 / #3175).", decl.Owner, got, "user-attacker", decl.Owner)
	}
	if payload["title"] != "forged" {
		t.Fatalf("the stamp touched a field that is not the declared owner: %v", payload)
	}
}

// The same claim from the other end: an ABSENT owner field is stamped
// rather than left absent. A row that cannot say who owns it is denied
// by the read gate and by #3174's write guard alike, so leaving it
// absent would quietly mint unreachable rows.
func TestRawInsertStampsAnAbsentOwner(t *testing.T) {
	decl := declFor(t, declaredOwnedConcept)
	payload := map[string]any{"title": "no owner supplied"}

	if err := stampRowAuthzOwner(callerCtx("user-a"), declaredOwnedConcept, payload, rawWrite); err != nil {
		t.Fatalf("the stamp refused an ordinary authenticated create: %v", err)
	}
	if got := payload[decl.Owner]; got != "user-a" {
		t.Fatalf("%s = %v, want %q", decl.Owner, got, "user-a")
	}
}

// AC 2: the stamp is driven by the DECLARATION, not by a hand-maintained
// concept list.
//
// This is the difference between this fix and the nine per-concept
// guards it sits beside. Those guards each cite a different incident and
// cover exactly the concept that incident named; this walks every concept
// the loader reports as owned-tier and asserts each one stamps its OWN
// declared field. A newly-declared concept is covered the moment it is
// declared, with no edit here or in the engine.
func TestTheStampIsDrivenByTheDeclarationNotAList(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}

	owned := 0
	for _, c := range memorynodes.List() {
		if c == nil || c.RowAuthz == nil || c.RowAuthz.Tier != langparser.RowAuthzOwned {
			continue
		}
		if c.RowAuthz.Owner == langparser.RowAuthzSelfOwnedField {
			// Self-owned: the row IS the owner, so there is no payload
			// field to stamp. See TestSelfOwnedConceptsHaveNoFieldToStamp.
			continue
		}
		owned++
		payload := map[string]any{c.RowAuthz.Owner: "user-victim"}
		if err := stampRowAuthzOwner(callerCtx("user-caller"), c.Name, payload, rawWrite); err != nil {
			t.Fatalf("%s: the stamp refused an authenticated create: %v", c.Name, err)
		}
		if got := payload[c.RowAuthz.Owner]; got != "user-caller" {
			t.Errorf("%s declares owner=%q and a raw insert still wrote %v. The stamp must "+
				"read the field out of the declaration, so a concept is covered the moment "+
				"it declares a tier (memql#3175 AC 2).", c.Name, c.RowAuthz.Owner, got)
		}
	}
	if owned == 0 {
		t.Fatal("no concept declares the owned tier -- this test would pass forever")
	}
	t.Logf("owned-tier concepts covered by the declaration-driven stamp: %d", owned)
}

// COVERAGE BOUNDARY: an undeclared concept is untouched, exactly as
// #3174's guard leaves it untouched. Widening the declared population is
// task #3173's job.
func TestTheStampLeavesUndeclaredConceptsAlone(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	undeclared := ""
	for _, c := range memorynodes.List() {
		if c != nil && c.RowAuthz == nil {
			undeclared = c.Name
			break
		}
	}
	if undeclared == "" {
		t.Skip("every loaded concept declares a tier; there is no undeclared boundary left")
	}
	payload := map[string]any{"ownerUserId": "user-victim"}
	if err := stampRowAuthzOwner(callerCtx("user-caller"), undeclared, payload, rawWrite); err != nil {
		t.Fatalf("a create on an UNDECLARED concept was refused: %v", err)
	}
	if payload["ownerUserId"] != "user-victim" {
		t.Fatalf("the stamp rewrote a field on an undeclared concept (%s). It reaches exactly "+
			"the concepts that declare an owner; the rest is task #3173.", undeclared)
	}
}

// The tiers with no owner FIELD are left alone, and each for its own
// stated reason rather than by falling off the end of a switch.
func TestTheStampLeavesTheOtherTiersAlone(t *testing.T) {
	// clusterOwner: administrative rows, no owner field. Who may write
	// them is #3174's question on an existing row and the read gate's on
	// the way out; there is nothing here to stamp.
	payload := map[string]any{"disposition": "completed"}
	if err := stampRowAuthzOwner(callerCtx("user-a"), declaredClusterOwnerConcept, payload, rawWrite); err != nil {
		t.Fatalf("a create on the clusterOwner tier was refused by the stamp: %v", err)
	}
	if len(payload) != 1 || payload["disposition"] != "completed" {
		t.Fatalf("the stamp rewrote a clusterOwner-tier payload: %v", payload)
	}
}

// The escape set is #3174's, not a second one.
//
// Two spellings of "who owns this row" is how a reader and a writer
// drift into disagreeing about the very thing they both exist to
// describe -- so this asserts the stamp DELEGATES rather than
// reimplements. A cluster owner and a per-write internal-origin stamp
// write the owner they supply; everybody else is stamped.
func TestTheStampReusesTheWriteGuardEscapeSet(t *testing.T) {
	decl := declFor(t, declaredOwnedConcept)
	cases := []struct {
		name    string
		ctx     context.Context
		escapes bool
	}{
		{"authenticated writer", callerCtx("user-attacker"), false},
		{"admin (NOT an escape -- #3174 constraint 2)", adminRoleCtx("user-admin"), false},
		{"cluster owner", ownerRoleCtx("user-operator"), true},
		{"internal origin", auth.ContextWithInternalOrigin(callerCtx("user-a")), true},
		{"explicit client origin", auth.ContextWithClientOrigin(callerCtx("user-a")), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := forgedOwnerPayload(t, "user-victim")
			if err := stampRowAuthzOwner(tc.ctx, declaredOwnedConcept, payload, rawWrite); err != nil {
				t.Fatalf("refused: %v", err)
			}
			kept := payload[decl.Owner] == "user-victim"
			if kept != tc.escapes {
				t.Fatalf("caller-supplied owner survived = %t, want %t. The escape set is "+
					"exactly {internal origin, cluster owner} and it is READ FROM "+
					"rowAuthzWriteEscape, not restated here (memql#3174).", kept, tc.escapes)
			}
		})
	}
}

// FINDING 4 of memql#3172, on the create path: no caller identity, no
// stamp -- and therefore no write.
//
// Stamping the empty string would be worse than doing nothing: it mints
// a row whose declared owner is "", which is a legal value that the
// owned-tier predicate MATCHES. #3172 refuses such a read and #3174
// refuses such a write; a create that manufactures the row in the first
// place has to refuse too, or the three disagree.
func TestTheStampRefusesACallerWithNoIdentity(t *testing.T) {
	payload := forgedOwnerPayload(t, "user-victim")
	err := stampRowAuthzOwner(context.Background(), declaredOwnedConcept, payload, rawWrite)
	if err == nil {
		t.Fatal("a caller with no identity created a row on a declared concept. There is no " +
			"actor to stamp, and stamping the empty string mints a row whose owner is a " +
			"value the owned-tier predicate matches -- refuse instead (memql#3172 finding 4).")
	}
	if !strings.Contains(err.Error(), "row-authz") {
		t.Fatalf("the refusal does not name row-authz: %v", err)
	}
}

// SCOPE: the template path is NOT restamped.
//
// A named mutation already answers this question in its own body --
// `stamp { ownerUserId: actor.userId }` -- and memql#2982 / #2988 /
// #2989 spent three PRs making that answer trustworthy. Restamping it
// here would silently overrule a mutation that legitimately writes a row
// for somebody else (a handler running under the document owner's actor,
// an operator provisioning flow), which is a policy change nobody asked
// for. The raw surface is the one with NO body to state an answer.
func TestTheStampLeavesTheMutationTemplatePathAlone(t *testing.T) {
	decl := declFor(t, declaredOwnedConcept)
	payload := map[string]any{decl.Owner: "user-other", "title": "from a template"}
	if err := stampRowAuthzOwner(callerCtx("user-caller"), declaredOwnedConcept, payload, templatedWrite); err != nil {
		t.Fatalf("refused: %v", err)
	}
	if payload[decl.Owner] != "user-other" {
		t.Fatalf("the stamp overruled a rendered mutation template. The template's own "+
			"`stamp { }` is the author's stated answer and #2982/#2988/#2989 made it "+
			"trustworthy; this fix is for the surface with no body to state one. Got %v",
			payload[decl.Owner])
	}
}

// THE FAIL-CLOSED DIRECTION of the template flag, stated as a test.
//
// MutationNode.FromTemplate is false in its zero value, so a MutationNode
// built by any future producer is treated as raw and gets stamped. The
// opposite spelling (`Raw bool`) would default a new producer to
// unstamped -- a new hole opened by omission, which is precisely how
// this one arrived.
func TestAMutationNodeIsStampedUnlessItSaysItCameFromATemplate(t *testing.T) {
	decl := declFor(t, declaredOwnedConcept)
	var zero MutationNode
	if zero.FromTemplate {
		t.Fatal("MutationNode.FromTemplate must be false in its zero value so an unknown " +
			"producer is treated as raw and stamped; the flag is fail-closed by direction")
	}
	payload := forgedOwnerPayload(t, "user-victim")
	if err := stampRowAuthzOwner(callerCtx("user-caller"), declaredOwnedConcept, payload, zero.FromTemplate); err != nil {
		t.Fatalf("refused: %v", err)
	}
	if payload[decl.Owner] != "user-caller" {
		t.Fatalf("a zero-value MutationNode was not stamped: %v", payload[decl.Owner])
	}
}

// THE WIRING PROOF, and the one test here that fails against the
// pre-change tree.
//
// Everything above proves the stamp answers correctly; this proves
// executeWrite CONSULTS it. It drives the real write path on a DB-free
// engine with a caller that has an actor (so mutationActor is satisfied)
// but no resolved identity (so the stamp must refuse). Before this
// change the same call sailed past every gate and stopped only at the
// store, with the payload exactly as supplied.
func TestExecuteWriteConsultsTheOwnerStamp(t *testing.T) {
	eng := stampProbeEngine(t)
	// An actor is present -- this is not the "no actor" rejection
	// mutationActor already performs. What is absent is the resolved
	// AccessContext the declared owner would be stamped from.
	ctx := rawInsertCtx(context.Background(), declaredOwnedConcept, "system:probe-3175")

	_, _, err := eng.executeWrite(ctx, rawInsert(declaredOwnedConcept, forgedOwnerPayload(t, "user-victim")), false)
	if err == nil {
		t.Fatal("a raw insert on a declared concept with no caller identity was accepted")
	}
	if !strings.Contains(err.Error(), "row-authz") {
		t.Fatalf("executeWrite refused for the wrong reason (%v). It must consult the owner "+
			"stamp BEFORE the row reaches storage; an error from the store means the raw "+
			"insert( path still writes the payload as supplied (memql#3059 / #3175).", err)
	}
}

// The legitimate case through the same real path: an authenticated
// caller's raw insert is NOT refused by the stamp, and gets as far as
// the store (which is where a DB-free engine stops). Without this the
// stamp could be a blanket refusal and the test above would still pass.
func TestExecuteWriteDoesNotRefuseAnAuthenticatedRawInsert(t *testing.T) {
	eng := stampProbeEngine(t)
	ctx := rawInsertCtx(callerCtx("user-caller"), declaredOwnedConcept, "user-caller")

	_, _, err := eng.executeWrite(ctx, rawInsert(declaredOwnedConcept, forgedOwnerPayload(t, "user-victim")), false)
	if err == nil {
		t.Skip("the write succeeded; this engine is expected to be DB-free")
	}
	if strings.Contains(err.Error(), "row-authz") {
		t.Fatalf("an authenticated caller's own create was refused: %v. The create path "+
			"STAMPS; refusing is #3174's job and only on an existing row.", err)
	}
	if !strings.Contains(err.Error(), "database") {
		t.Fatalf("the create stopped somewhere other than the store: %v", err)
	}
}

// SELF-OWNED (`@rowAuthz(owner="id")`) has no payload field to stamp:
// the row's identity IS the owner, and the id is not something the
// server can rewrite without changing which row is being written.
//
// Zero concepts declare it today, so the stamp's silence there is inert.
// This gate says so out loud and fails the day one does -- the same
// shape #3174 uses for the `granted` tier, so the gap is adjudicated at
// merge rather than discovered in production.
func TestSelfOwnedConceptsHaveNoFieldToStamp(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	for _, c := range memorynodes.List() {
		if c == nil || c.RowAuthz == nil || c.RowAuthz.Tier != langparser.RowAuthzOwned {
			continue
		}
		if c.RowAuthz.Owner == langparser.RowAuthzSelfOwnedField {
			t.Errorf("%s declares the self-owned form `@rowAuthz(owner=%q)`. There is no "+
				"payload field to stamp -- the row's id IS the owner -- so a raw insert "+
				"naming somebody else's id still mints a row under their identity. "+
				"Adjudicate before merging: either the concept takes the field form, or "+
				"the stamp grows an id-side rule (memql#3175).",
				c.Name, langparser.RowAuthzSelfOwnedField)
		}
	}
}

// AC 4: the nine per-concept guards are DEFENCE IN DEPTH, not something
// this replaced.
//
// Each cites its own incident (#403, #2070, #2072, #2140, #2143, #2513,
// #1787, #1158) and each covers a rule this stamp says nothing about --
// state machines, role ranks, credential kinds. The generic stamp
// answers only "who owns this row". So the guards stay, and this asserts
// their call sites are still there rather than trusting that nobody
// removed them while adding the generic form.
func TestThePerConceptGuardsStillFire(t *testing.T) {
	source := readExecutorMutationSource(t)
	// concept const -> the guard call that must still be dispatched for it.
	guards := map[string][]string{
		"memorynodes.ConceptCognitionUtterance":   {"validateCognitionUtteranceWriteAuthorization("},
		"memorynodes.ConceptCognitionParticipant": {"validateAndStampParticipantPayload("},
		"conceptAgentsAgent":                      {"validateAgentLockedItems(", "validateAgentKindActorScope("},
		"conceptRbacRole":                         {"validateRbacBaseRoleImmutable(", "validateRbacCustomRoleRankBound("},
		"conceptIdentityIdentity":                 {"validateIdentityCredentialActorScope("},
		"conceptHealingOverride":                  {"validateHealingBaseImmutable(", "validateHealingValidationRankBound("},
		"memorynodes.ConceptHarnessStep":          {"validateHarnessStepTransition("},
		"conceptPlannerPlan":                      {"validateFeedbackIntakeTransition("},
		"conceptForgeRequest":                     {"validateForgeRequestTransition("},
	}
	names := make([]string, 0, len(guards))
	for name := range guards {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, conceptConst := range names {
		if !strings.Contains(source, "conceptMeta.Name == "+conceptConst) {
			t.Errorf("executeWrite no longer dispatches on %s. The nine per-concept guards "+
				"are defence in depth and the generic owner stamp did NOT replace them: "+
				"they enforce state machines, role ranks and credential kinds, none of "+
				"which is 'who owns this row' (memql#3175 AC 4).", conceptConst)
			continue
		}
		for _, call := range guards[conceptConst] {
			if !strings.Contains(source, call) {
				t.Errorf("%s: the guard call %s is gone from executeWrite", conceptConst, call)
			}
		}
	}
	t.Logf("per-concept guards still dispatched from executeWrite: %s", strconv.Itoa(len(names)))
}

// The stamp runs BEFORE the row reaches storage, and after the
// read-merge -- so the value it writes is the one that lands rather than
// one a later merge step can displace. Asserted on the source because
// the ordering is the property, and a DB-free engine cannot observe the
// stored row.
func TestTheStampRunsAfterTheReadMergeAndBeforeTheStore(t *testing.T) {
	source := readExecutorMutationSource(t)
	stamp := strings.Index(source, "stampRowAuthzOwner(ctx,")
	merge := strings.Index(source, "mergePayloadFields(priorPayload, payload,")
	store := strings.Index(source, "conceptMeta.Create(ctx, store, createParams)")
	if stamp < 0 || merge < 0 || store < 0 {
		t.Fatalf("could not locate the ordering landmarks in executor_mutation.go "+
			"(stamp=%d merge=%d store=%d)", stamp, merge, store)
	}
	if !(merge < stamp && stamp < store) {
		t.Fatalf("the owner stamp must run AFTER the read-merge (which replaces the payload "+
			"map wholesale with the merged prior row) and BEFORE the store; got "+
			"merge=%d stamp=%d store=%d", merge, stamp, store)
	}
}

// AC 6: on the TEMPLATE path, `stamp` beats `accept` when both name the
// same key -- asserted rather than assumed.
//
// The behaviour is real but emergent, and nothing pinned it. The
// rewriter emits accept's auto-bound lines first and stamp's lines
// after (component/language/parser/rewriter.go), and parseObjectLiteral
// is last-key-wins, so the server-set value overwrites the caller-bound
// one. That is the SAFE direction -- and it is exactly the direction the
// raw-path stamp above takes for the surface that has no `stamp { }` to
// win with, so the two surfaces agree. The only related check that
// existed was the duplicate-`id:` rejection in the rewriter, which says
// nothing about any other key.
//
// Driven through the real loader and the real renderer: the claim is
// about what the pipeline produces, and a hand-built template would be
// asserting on a fixture instead.
func TestStampBeatsAcceptOnAKeyCollision(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	decl := declFor(t, declaredOwnedConcept)

	// Both blocks name the declared owner field. accept binds it from the
	// CALLER's arg; stamp sets it from the actor.
	src := `@actor
mutate note collisionProbe {
  args {
    noteId       string  @required
    ownerUserId  string  @required
    body         string  @required
  }
  insert {
    accept { ownerUserId, body }
    stamp {
      id: args.noteId
      ownerUserId: actor.userId
    }
  }
}`
	fn, err := tryParseNewFunctionSyntax("collisionProbe", "mutation", src, "test.memql", memorynodes.DefaultRegistry())
	if err != nil {
		t.Fatalf("loading the collision probe: %v", err)
	}
	if fn.MutationTemplate == nil {
		t.Fatal("the probe loaded without a mutation template")
	}

	eng := &MemQLEngine{}
	node, err := eng.renderMutationTemplate(callerCtx("user-actor"), fn.MutationTemplate, map[string]any{
		"noteId":      "note-collision",
		"ownerUserId": "user-victim",
		"body":        "b",
	})
	if err != nil {
		t.Fatalf("rendering the collision probe: %v", err)
	}
	var payload map[string]any
	if uerr := json.Unmarshal([]byte(node.PayloadRaw), &payload); uerr != nil {
		t.Fatalf("unmarshal rendered payload: %v", uerr)
	}
	if got := payload[decl.Owner]; got != "user-actor" {
		t.Fatalf("%s = %v, want the STAMPED actor value %q. `accept` and `stamp` naming the "+
			"same key must resolve stamp-wins: the rewriter emits accept's lines first and "+
			"the object literal is last-key-wins, so the server-set value overwrites the "+
			"caller-bound one. If this ever flips, every mutation that both accepts and "+
			"stamps an authz field starts taking the caller's word for it (memql#3175 AC 6).",
			decl.Owner, got, "user-actor")
	}
	// CONTROL 1: the caller's OTHER accepted field is untouched, so the
	// assertion above is about precedence rather than accept being broken.
	if payload["body"] != "b" {
		t.Fatalf("body = %v, want the caller's accepted value; the probe is not measuring "+
			"precedence if accept never bound anything", payload["body"])
	}

	// CONTROL 2: the same probe WITHOUT the stamp line renders the
	// caller's value. This is what makes the assertion above evidence of
	// precedence -- accept genuinely binds this key, and stamp is what
	// displaces it, rather than accept having quietly dropped it.
	noStamp := strings.Replace(src, "      ownerUserId: actor.userId\n", "", 1)
	noStamp = strings.Replace(noStamp, "collisionProbe", "collisionControlProbe", 1)
	controlFn, err := tryParseNewFunctionSyntax("collisionControlProbe", "mutation", noStamp, "test.memql", memorynodes.DefaultRegistry())
	if err != nil {
		t.Fatalf("loading the control probe: %v", err)
	}
	controlNode, err := eng.renderMutationTemplate(callerCtx("user-actor"), controlFn.MutationTemplate, map[string]any{
		"noteId":      "note-collision-control",
		"ownerUserId": "user-victim",
		"body":        "b",
	})
	if err != nil {
		t.Fatalf("rendering the control probe: %v", err)
	}
	var controlPayload map[string]any
	if uerr := json.Unmarshal([]byte(controlNode.PayloadRaw), &controlPayload); uerr != nil {
		t.Fatalf("unmarshal control payload: %v", uerr)
	}
	if controlPayload[decl.Owner] != "user-victim" {
		t.Fatalf("the control did not bind %s from caller args (%v), so the collision "+
			"assertion above proves nothing", decl.Owner, controlPayload[decl.Owner])
	}
}

// The rendered node also carries FromTemplate, which is what keeps the
// raw-path stamp from overruling the collision resolved above.
func TestARenderedTemplateIsMarkedAsComingFromATemplate(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	src := `mutate note templateFlagProbe {
  args {
    noteId  string  @required
    body    string  @required
  }
  insert {
    id: args.noteId
    args.body
  }
}`
	fn, err := tryParseNewFunctionSyntax("templateFlagProbe", "mutation", src, "test.memql", memorynodes.DefaultRegistry())
	if err != nil {
		t.Fatalf("loading the probe: %v", err)
	}
	eng := &MemQLEngine{}
	node, err := eng.renderMutationTemplate(context.Background(), fn.MutationTemplate, map[string]any{
		"noteId": "note-flag", "body": "b",
	})
	if err != nil {
		t.Fatalf("rendering the probe: %v", err)
	}
	if !node.FromTemplate {
		t.Fatal("renderMutationTemplate produced a node that does not say it came from a " +
			"template. The raw-path owner stamp would then overrule the mutation's own " +
			"`stamp { }` block on every named mutation (memql#3175).")
	}
}

func readExecutorMutationSource(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("executor_mutation.go"))
	if err != nil {
		t.Fatalf("read executor_mutation.go: %v", err)
	}
	return string(body)
}
