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
// It does not enforce anything at read time; no predicate is injected
// and no query result changes (memql#2920/#2921 remain inert). It
// gates the CONSISTENCY of a declaration against the write path.
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
var ownerGateExemptions = map[string]string{}
