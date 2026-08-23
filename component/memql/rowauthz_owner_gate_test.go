package memql

import (
	"sort"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// rowauthz_owner_gate_test.go -- memql#2982.
//
// Homed in package memql rather than package dsl, where the sibling
// authz gates live, because this one reads the LOADED MutationTemplates
// (see below) and nothing outside this package can construct a function
// registry. Exporting a constructor purely for a test would widen the
// public surface to buy a file move.
//
// `@rowAuthz(owner="F")` asserts that F identifies the row's owner.
// That assertion is worthless if a caller can write F -- and worse than
// worthless, because it is false in the direction that reads as safe: a
// reader auditing the tier sees a declared owner and stops looking.
//
// This gate makes the declaration mean something on the write path.
// Every concept declaring an owner tier must have that field
// server-stamped from `actor.userId` and unwritable from caller args
// through ANY mutation.
//
// # WHY IT DERIVES FROM LOADED TEMPLATES
//
// The natural check -- "is F named in an `accept { ... }` block" --
// misses the live cases, and one of them it cannot see at all:
//
//   - `appendDocumentVersion` wrote a bare `args.ownerUserId` mirror in
//     a longhand `insert { }` with no `accept` block anywhere. That was
//     memql#2989; it stamps from `actor.userId` now, but the shape is
//     still expressible and a source scanner would still miss it.
//   - `updateCalendarEvent` splatted `args.payload` with NO overlay, so
//     the field was caller-writable without appearing near an `accept`
//     block. That was memql#2988, a live defect on a concept that
//     declared the tier and whose field doc called it "the load-bearing
//     per-row authz guard".
//
// `updateNote` and `updateCalendarEvent` were near-identical in source
// and differed entirely in whether memql#401's overlay-wins protection
// engaged, because that turns on which explicit fields survive the
// loader's hoist-and-delete pass. A source scanner would have to
// re-derive that rule. `OwnerFieldProvenance` reads the loaded
// `MutationTemplate` instead, which already carries its outcome -- the
// memql#2875 lesson a third time.
//
// # WHAT THIS GATE IS NOT
//
// It is a static check and not a runtime one: THIS gate injects no
// predicate and changes no result set. That is a statement about the
// gate. The parenthetical here used to extend it to memql#2920/#2921
// as a whole, which stopped being true when Phase 3 landed read-path
// enforcement (memql#3172; swept in memql#3987) -- a declared tier now
// does change what reads return, and TestRowAuthzEnforcementLandGate
// is what watches that. What this gate does is check the CONSISTENCY
// of a declaration against the write path.
func TestDeclaredOwnerFieldsAreServerStamped(t *testing.T) {
	// Load SKIPS are load-bearing here, and discarding them is the
	// mistake memql#2909 was filed to fix -- "memqllint's engine-parity
	// pass ran this very code and discarded the answer, so a product
	// bundle with a mistyped property linted clean and lost a concept at
	// boot."
	//
	// Concretely: delete the `@actor` line from updateCalendarEvent and
	// #2621's validateActorBinding fails the parse, so the mutation is
	// silently skipped, the gate sees only the stamping create, and the
	// concept PASSES. A gate that cannot see a construct cannot judge
	// it, and silence must not read as a pass.
	conceptCount, conceptSkips, err := LoadUnifiedConceptsWithSkips(nil)
	if err != nil {
		t.Fatalf("LoadUnifiedConceptsWithSkips: %v", err)
	}
	if len(conceptSkips) > 0 {
		t.Fatalf("%d concept(s) failed to load, so this gate cannot see them: %v",
			len(conceptSkips), conceptSkips)
	}
	if conceptCount == 0 {
		t.Fatal("no concepts loaded")
	}

	registry := newFunctionRegistry()
	report := newLoadReport()
	if _, _, err := LoadUnifiedFunctions(nil, registry, memoryNodes.DefaultRegistry(), report); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	if len(report.Skipped) > 0 {
		t.Fatalf("%d construct(s) were skipped at load, so this gate cannot see them and would "+
			"report a PASS on a concept whose forging mutation simply failed to parse:\n  %v",
			len(report.Skipped), report.Skipped)
	}

	declared := map[string]string{}
	for id, c := range memoryNodes.All() {
		if c == nil || c.RowAuthz == nil || c.RowAuthz.Tier != langparser.RowAuthzOwned {
			continue
		}
		declared[id] = c.RowAuthz.Owner
	}
	if len(declared) == 0 {
		t.Fatal("no concept declares an owner tier -- either the tree failed to load or the " +
			"@rowAuthz declarations are gone. A gate over nothing passes for the wrong reason.")
	}

	results := OwnerFieldProvenance(registry, declared)
	if len(results) != len(declared) {
		t.Fatalf("got %d verdicts for %d declared owner tiers", len(results), len(declared))
	}

	seenExempt := map[string]bool{}
	passed := 0
	for _, r := range results {
		if r.ServerStamped {
			passed++
			if reason, exempt := ownerGateExemptions[r.Concept]; exempt {
				t.Errorf("%s is on the exemption list (%s) but now PASSES. Delete the entry -- a "+
					"stale exemption hides the next regression on this concept.", r.Concept, reason)
			}
			continue
		}
		if reason, exempt := ownerGateExemptions[r.Concept]; exempt {
			seenExempt[r.Concept] = true
			t.Logf("KNOWN (exempt): %s.%s -- %s\n    tracked: %s",
				r.Concept, r.Field, r.Reason, reason)
			continue
		}
		t.Errorf(`%s declares @rowAuthz(owner=%q), but a caller can write that field.

  %s
  stamped by:  %v
  writable by: %v

A declared owner tier asserts the field identifies the row's owner. If a caller supplies it,
the declaration records a guarantee nothing provides -- and it reads as safe, so an auditor
seeing the tier stops looking.

Two fixes, and they are different decisions:
  - Re-stamp the field from the actor in every write, as updateNote does and as memql#2988's
    fix did for updateCalendarEvent. Correct when the owner is always the caller.
  - Drop the owner tier, if these rows are not really per-user-owned.

Do NOT add an exemption to silence this without filing the decision -- the existing entries
each carry an issue number for exactly that reason.`,
			r.Concept, r.Field, r.Reason, r.StampedBy, r.WritableBy)
	}

	// A stale exemption is as bad as a missing gate: it names a concept
	// that may no longer exist, and it suppresses that concept forever.
	var stale []string
	for concept := range ownerGateExemptions {
		if _, declaredNow := declared[concept]; !declaredNow {
			stale = append(stale, concept+" (no longer declares an owner tier)")
			continue
		}
		if !seenExempt[concept] {
			stale = append(stale, concept+" (declared, but the gate did not evaluate it)")
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("exemption list is stale:\n  %s\nRemove entries that no longer apply.",
			strings.Join(stale, "\n  "))
	}

	if passed == 0 {
		t.Error("every declared owner tier is exempt, so this gate proves nothing. If that is " +
			"genuinely the state of the tree, the tier means nothing and the program needs a " +
			"different answer than an exemption list.")
	}
}

// ownerGateExemptions are the concepts that declare an owner tier over
// a caller-supplied field TODAY, grandfathered so the gate can hard-fail
// on anything new.
//
// EMPTY, and that is the point. It held two entries -- `library.generatedOutput`
// and `library.documentVersion`, both memql#2989 -- and both were fixed
// rather than annotated: their three mutations now stamp `ownerUserId`
// from `actor.userId`, and every Go call site already ran under the
// owner's actor, so no written value changed and the field simply
// stopped being forgeable.
//
// An empty map means every declared owner tier in the tree is
// server-stamped, which is the precondition memql#2803 records for
// ruling on read-time enforcement: a tier over a caller-writable field
// would be enforcement the attacker sets.
//
// Two rules for anyone tempted to add an entry back:
//
//   - Fix it instead if you can. memql#2989's two entries looked like
//     they needed a language change (`@serverOnly` on mutations, then
//     an internal-origin stamp at five call sites) and that route was
//     built and REFUTED -- stamping internal origin on a request-derived
//     context opens every `@serverOnly` construct for the rest of that
//     request. The actual fix was three stamp lines.
//   - If you must exempt, file the decision first and reference it here.
//     Adding an entry without one is how a gate turns into decoration.
//
// EMPTY AGAIN as of memql#3348, and the round trip is worth recording
// because it is the one the header above argues for.
//
// memql#3323 added a single entry -- `v1:campaigns:delivery` -- for the
// OTHER failure this gate reports: not "a caller can write the field"
// but "nothing writes this concept at all", so the declared owner tier
// rested on however a row happened to be seeded. The two remedies the
// error message offers both misfired on it. Re-stamping from the actor
// had nothing to attach to. Dropping the tier was wrong: delivery rows
// are genuinely one operator's, they carry a recipient's address under
// @pii, and dropping it would have moved the failure onto the
// undeclared gate while leaving the rows LESS protected.
//
// The entry was therefore held open against the issue that would supply
// the missing writer, rather than closed by inventing a `recordDelivery`
// mutation to turn the gate green. memql#3348 supplied it:
// `recordCampaignDelivery` (dsl/campaigns/mutations.memql) stamps
// `ownerUserId` from `actor.userId`, and component/campaigns' drain
// worker calls it under the CAMPAIGN OWNER'S actor -- so the value is
// the campaign's owner, derived from a row the caller had already
// proved they could read, and no caller argument can name a different
// one. The exemption then had to come out, and the stale-entry check
// below is what forced the issue: the gate fails on an entry that now
// passes.
// NOT EMPTY ANY MORE, as of memql#4340, and the entry below is a
// deliberate re-opening rather than an oversight. Read the two rules
// above first; this is the "file the decision, then reference it" case.
//
// WHAT THE ENTRY IS. v1:library:artifact declares the composite tier
// (`owner="ownerUserId", clusterOwner`) because the Artifacts page became
// a primary surface and the concept was in the undeclared population --
// unmeasured on the read side, on the subscription side, and on graph
// expansion. Declaring it closes all three. It does NOT close the write
// side, and this entry is what refuses to pretend otherwise.
//
// WHY THE TWO OFFERED REMEDIES BOTH MISFIRE, which is the part worth
// having in the file rather than in a PR comment:
//
//   - "Re-stamp from the actor" is WRONG here, not merely awkward.
//     createArtifact is invoked from the six index*OnCreate promotion
//     automations, and an event-triggered automation runs on a context
//     the scheduler builds with context.Background()
//     (component/automations/scheduler.go), so contextWithSystemActor
//     stamps `automation:<name>` as actor.userId -- asserted, not
//     inferred, by TestSystemActorStampsABindableAccessContext. Stamping
//     would therefore write the AUTOMATION as the owner of every promoted
//     row, which is worse than a forgeable field: it is a wrong one, on
//     every row, silently.
//   - "Drop the owner tier" is wrong for the reason memql#3323's
//     v1:campaigns:delivery entry was: these rows are genuinely one
//     person's, they are the index behind a per-user page, and dropping
//     the tier moves the failure back onto the undeclared gate while
//     leaving the rows LESS protected than they are with it.
//
// WHAT IS ACTUALLY TRUE ABOUT THE FIELD TODAY. Every live writer derives
// ownerUserId from a row it did not invent: the promotion automations
// bind it off the CDC payload of the row the engine itself just wrote,
// and integrations/library's touchArtifact, label capabilities and
// analysis re-stamp read it off the current artifact or file row. None
// takes it from a caller argument. But createArtifact is a registered
// mutation and is not @serverOnly, so an authenticated caller CAN name an
// arbitrary owner -- that hole predates this entry and the tier neither
// opens nor closes it.
//
// @serverOnly WAS TRIED AND REVERTED (memql#4340), and the reason is worth
// keeping: it forces every Go writer to stamp internal origin, and all of
// them run on REQUEST-DERIVED contexts (the label capabilities and the
// document edit path are reached from the portal and from agent tools).
// TestOnlyAllowlistedPackagesStampInternalOrigin exists to refuse exactly
// that shape. Trading a bounded forgery for a standing origin-laundering
// seam in a package that also hosts agent-callable builtins is the worse
// side of the trade, so the honest exemption stays.
//
// WHAT WOULD CLOSE IT, so the next reader does not have to re-derive it:
// the promotion write has to run AS the owner rather than on their behalf
// -- the shape component/campaigns' drain worker uses for
// recordCampaignDelivery, where the Go caller resolves the owner off a row
// and calls under auth.ContextWithUserActor. For promotion that means the
// automation handing off to a Go seam that resolves the owner from the
// SOURCE ROW (not from an argument) and re-enters under it, at which point
// createArtifact can stamp and this entry must come out. That is a
// redesign of the write path for all six promotions, which is exactly what
// memql#2803 has recorded as the blocker since before the tier existed --
// and doing it as a side effect of declaring the tier would bury a
// six-automation behaviour change inside an annotation.
//
// Moving the ownerUserId argument from the mutation to a builtin would
// turn this gate green and change nothing about forgeability. That is the
// "gate turns into decoration" outcome the header warns about, and it is
// the reason this is an honest entry instead.
var ownerGateExemptions = map[string]string{
	"v1:library:artifact": "memql#4340 -- the composite tier lands with the Artifacts page; createArtifact " +
		"threads ownerUserId from the promoting automation's SOURCE row because an event-triggered " +
		"automation's actor is the system principal, not the owner. The write-as-the-owner redesign " +
		"is memql#2803's recorded blocker and is tracked there.",
}
