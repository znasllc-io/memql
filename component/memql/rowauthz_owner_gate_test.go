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
//   - `appendDocumentVersion` writes a bare `args.ownerUserId` mirror in
//     a longhand `insert { }` with no `accept` block anywhere.
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
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(nil, registry, memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
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
// This list is meant to shrink to empty. Each entry carries the issue
// tracking its decision -- and the decision is genuinely open, not
// paperwork: both concepts' field docs claim edits "run server-side on
// the owner's behalf", and NOTHING ENFORCES THAT. The tree has zero
// `@serverOnly` mutations, so "handler-invoked only" is not expressible
// on a write at all. The gate therefore cannot tell "handler-invoked,
// safe" from "client-callable, forgeable", and deliberately
// over-rejects rather than guess.
//
// Adding an entry here without filing the decision is how a gate turns
// into decoration.
var ownerGateExemptions = map[string]string{
	"v1:library:generatedOutput": "memql#2989 -- createGeneratedOutput/updateGeneratedOutputContent " +
		"accept ownerUserId from caller args; the field doc claims handler-invoked, which " +
		"@serverOnly cannot express on a mutation today",
	"v1:library:documentVersion": "memql#2989 -- appendDocumentVersion writes a bare args.ownerUserId " +
		"mirror; same unenforceable 'server-side on the owner's behalf' claim",
}
