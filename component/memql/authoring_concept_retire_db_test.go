package memql

// authoring_concept_retire_db_test.go -- the halves of concept demote
// (memql#3756) that a fake cannot vouch for, against a REAL engine and a REAL
// Postgres: the row count itself, and the claim that a retired concept's rows are
// still readable.
//
// Everything about the DECISION is unit-tested with an injected counter in
// authoring_concept_retire_test.go. What is left here is precisely what that seam
// stands in for:
//
//   - the count is taken under NO actor, so it does not vary with who asked. A
//     count that varied would make the outcome depend on the caller, and an owner
//     who wrote nothing while a colleague wrote a thousand rows would REMOVE the
//     definition those rows are addressed by;
//   - reads still work after the retirement, through the engine, not just as
//     bytes in a table.
//
// Postgres-gated: skips when no DB is reachable, exactly like the other _db_
// tests in this package. The fixtures carry a per-process unique namespace so
// concurrent runs (and other agents sharing the dev database) never collide, and
// nothing here truncates anything.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// retireDBFixture is one self-contained authored bundle: a concept, a mutation
// that writes rows under it, and a query that reads them back. The namespace is
// unique per process so the canonical ids cannot collide with a sibling run.
type retireDBFixture struct {
	namespace string
	conceptId string
	bundle    string
	concept   string
}

func newRetireDBFixture(suffix string) retireDBFixture {
	ns := "retire3756" + strings.ReplaceAll(suffix, "-", "")
	conceptSrc := fmt.Sprintf(`@version("1.0.0")
@namespace(%q)
@description("A concept taught to a running cluster, then withdrawn")
concept demoWidget {
  ownerUserId  string
  label        string
}`, ns)
	mutationSrc := fmt.Sprintf(`use %s.concepts.{ demoWidget }

@description("Create a demo widget")
@actor
mutate demoWidget createDemoWidget {
  args {
    widgetId  string  @required
    label     string  @required
  }
  insert {
    id:          canonicalId(args.widgetId, demoWidget)
    label:       args.label
    ownerUserId: actor.userId
  }
}`, ns)
	querySrc := fmt.Sprintf(`use %s.concepts.{ demoWidget }

@description("Every demo widget this caller owns")
@unbounded("fixture query over a per-test concept")
@actor
query demoWidget demoWidgetsAll {
  filter  ownerUserId==actor.userId
}`, ns)
	return retireDBFixture{
		namespace: ns,
		conceptId: "v1:" + ns + ":demoWidget",
		concept:   conceptSrc,
		bundle:    conceptSrc + "\n\n" + mutationSrc + "\n\n" + querySrc,
	}
}

// retireDBEngine boots the real engine against the real Postgres and restores the
// package-level concept registry afterwards, so a promoted concept never leaks
// into another test in this package.
//
// It binds the DEFAULT registry deliberately: the Gate-1 sandbox clones the
// package default when it binds a bundle's constructs, so only an engine bound to
// that same registry reproduces the production binding of a mutation to a
// just-promoted concept.
func retireDBEngine(t *testing.T) (*MemQLEngine, context.Context) {
	t.Helper()
	before := memoryNodes.All()
	t.Cleanup(func() { memoryNodes.ReplaceAll(before) })
	eng, _, ctx := readMergeTestEngine(t)
	return eng, ctx
}

// promoteBundleIntoEngine validates a bundle and promotes every construct in it
// into the engine's SHARED registries -- the in-process half of a durable
// promote, which is all these tests need: what they exercise is the live
// registry, the write path and the row count, none of which the persisted rows
// participate in. (The persistence + restart half is covered against fake stores
// in authoring_concept_retire_test.go.)
//
// It deliberately does NOT go through promoteConstructDurableWithStore's
// production store, which builds its persist calls in the `name({...})`
// object-literal form the grammar REMOVED in memql#2335 and the parser now
// refuses -- a pre-existing break in the durable promote's persistence that is
// invisible to every fake-store test and is not this issue's to fix.
func promoteBundleIntoEngine(t *testing.T, e *MemQLEngine, owner, source string) {
	t.Helper()
	reg := NewAuthoredRuntimeRegistry()
	res, err := AuthorSessionBundle(reg, owner, source, "")
	if err != nil {
		var detail []string
		for _, d := range res.Diagnostics {
			if !d.OK && strings.TrimSpace(d.Error) != "" {
				detail = append(detail, d.Error)
			}
		}
		t.Fatalf("author bundle: %v: %s", err, strings.Join(detail, "; "))
	}
	for _, c := range SplitBundleSource(source) {
		ac, ok := reg.Lookup(owner, c.Kind, c.Name)
		if !ok {
			t.Fatalf("session define did not register %s %q", c.Kind, c.Name)
		}
		if perr := e.PromoteAuthoredConstruct(context.Background(), ac); perr != nil {
			t.Fatalf("promote %s %q: %v", c.Kind, c.Name, perr)
		}
	}
}

// TestConceptRowCount_DoesNotVaryWithTheActor is the acceptance box that decides
// whether the other four are worth anything.
//
// Rows are written by user A. The count is then taken with user B in context, and
// with user A in context, and both must see BOTH rows. A count routed through an
// owner-scoped read would answer 0 for B -- and 0 is the answer that REMOVES the
// definition and leaves A's rows addressed by a name the engine no longer knows.
//
// The zero case is asserted too, on a concept nothing was written to, so the test
// shows the instrument can move: without it, "2 == 2" would also pass against a
// counter that returned a constant.
func TestConceptRowCount_DoesNotVaryWithTheActor(t *testing.T) {
	eng, ctx := retireDBEngine(t)
	fx := newRetireDBFixture(uniqueSuffix("count"))

	promoteBundleIntoEngine(t, eng, "owner-a", fx.bundle)

	// Two rows, both written by user A.
	userA := auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "owner-a", Role: auth.RoleWriter})
	for i, id := range []string{"w1-" + fx.namespace, "w2-" + fx.namespace} {
		runMutation(t, userA, eng, "createDemoWidget", map[string]any{
			"widgetId": id,
			"label":    fmt.Sprintf("row-%d", i),
		})
	}

	userB := auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "someone-else", Role: auth.RoleWriter})
	asB, err := eng.countConceptRows(userB, fx.conceptId)
	require.NoError(t, err, "counting rows as a user who wrote none")
	asA, err := eng.countConceptRows(userA, fx.conceptId)
	require.NoError(t, err, "counting rows as the user who wrote them")

	require.Equal(t, int64(2), asB,
		"the row count varied with the caller: as a user who wrote none of them it saw %d of 2. A demote taken on that count would REMOVE a concept whose rows exist", asB)
	require.Equal(t, asA, asB, "the row count must not depend on who asked")

	// The instrument can move: a concept with nothing under it counts zero.
	empty := newRetireDBFixture(uniqueSuffix("countempty"))
	promoteBundleIntoEngine(t, eng, "owner-a", empty.concept)
	zero, err := eng.countConceptRows(userB, empty.conceptId)
	require.NoError(t, err)
	require.Equal(t, int64(0), zero, "a concept nothing was ever written to must count zero")
}

// TestDemoteConcept_EndToEnd_RetiredStaysReadableAndRefusesWrites is the
// acceptance path against the real thing: promote, write, demote, and then the
// two claims that make "retired" mean anything -- the rows are still readable
// THROUGH THE ENGINE, and a write is refused naming the retirement.
//
// It demotes the CONCEPT alone rather than the bundle, so the query stays
// registered and can be used to prove the reads still resolve. A bundle demote
// would have withdrawn the query too, and "the rows are readable" would then have
// no instrument to be observed with.
func TestDemoteConcept_EndToEnd_RetiredStaysReadableAndRefusesWrites(t *testing.T) {
	eng, ctx := retireDBEngine(t)
	fx := newRetireDBFixture(uniqueSuffix("e2e"))

	promoteBundleIntoEngine(t, eng, "owner-a", fx.bundle)
	userA := auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "owner-a", Role: auth.RoleWriter})
	runMutation(t, userA, eng, "createDemoWidget", map[string]any{
		"widgetId": "e2e-" + fx.namespace,
		"label":    "before the demote",
	})

	outcome, err := eng.demoteAuthoredConstructWithOutcome(ctx, "concept", "demoWidget")
	if err != nil {
		t.Fatalf("demote concept: %v", err)
	}
	if outcome.Outcome != DemoteOutcomeRetired || outcome.RowCount != 1 {
		t.Fatalf("outcome = %+v, want retired with 1 row (the row that was written)", outcome)
	}

	// READS STILL WORK. Through the engine, through the promoted query, after
	// the definition was withdrawn.
	res, err := eng.Execute(userA, "demoWidgetsAll()")
	if err != nil {
		t.Fatalf("reading a retired concept's rows failed: %v -- retiring is supposed to keep them readable", err)
	}
	if res == nil || res.Bundle == nil || len(res.Bundle.Nodes) != 1 {
		t.Fatalf("read back %v rows from the retired concept, want 1", res)
	}

	// WRITES ARE REFUSED, by name.
	_, werr := eng.Execute(userA, fmt.Sprintf(`mutation createDemoWidget(widgetId: %s, label: "after the demote")`,
		languageParser.QuoteString("e2e2-"+fx.namespace)))
	if werr == nil {
		t.Fatal("a write to a retired concept was accepted")
	}
	if !strings.Contains(werr.Error(), "RETIRED") || !strings.Contains(werr.Error(), fx.conceptId) {
		t.Errorf("refusal = %q, want it to name the concept and the retirement", werr)
	}

	// RE-PROMOTING UN-RETIRES, and writes resume.
	promoteBundleIntoEngine(t, eng, "owner-a", fx.concept)
	runMutation(t, userA, eng, "createDemoWidget", map[string]any{
		"widgetId": "e2e3-" + fx.namespace,
		"label":    "after the re-promote",
	})
	res, err = eng.Execute(userA, "demoWidgetsAll()")
	require.NoError(t, err)
	require.Len(t, res.Bundle.Nodes, 2, "the write after the re-promote did not land")
}

// TestDemoteConcept_EndToEnd_ZeroRowsRemovesAndFreesTheName: the other row of the
// decision table against the real persistence. A concept nothing was ever written
// to is removed outright, and the name can be claimed again -- which is checked by
// claiming it, since "the registry entry is gone" and "the name is available" are
// different claims and only the second is what the outcome promises.
func TestDemoteConcept_EndToEnd_ZeroRowsRemovesAndFreesTheName(t *testing.T) {
	eng, ctx := retireDBEngine(t)
	fx := newRetireDBFixture(uniqueSuffix("empty"))

	promoteBundleIntoEngine(t, eng, "owner-a", fx.concept)
	if _, err := eng.concepts.Get(fx.conceptId); err != nil {
		t.Fatalf("pre-condition: the promoted concept is not registered: %v", err)
	}

	outcome, err := eng.demoteAuthoredConstructWithOutcome(ctx, "concept", "demoWidget")
	if err != nil {
		t.Fatalf("demote concept: %v", err)
	}
	if outcome.Outcome != DemoteOutcomeRemoved || outcome.RowCount != 0 {
		t.Fatalf("outcome = %+v, want removed with a zero row count", outcome)
	}
	if _, err := eng.concepts.Get(fx.conceptId); err == nil {
		t.Error("the concept is still registered after a zero-row demote")
	}

	promoteBundleIntoEngine(t, eng, "owner-a", fx.concept)
	if _, err := eng.concepts.Get(fx.conceptId); err != nil {
		t.Errorf("re-promote reported success but the concept is not registered: %v", err)
	}
}
