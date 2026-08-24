package memql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// The shipped mirror: v1:shopify:shopifyProduct declares
// @origin("shopify") (epic memql#4378, D8).
const testMirrorConcept = "v1:shopify:shopifyProduct"

// testEngineOverLoadedTree returns an engine whose registry is the
// embedded DSL tree, which is where the shipped declarations live.
func testEngineOverLoadedTree(t *testing.T) *MemQLEngine {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	return &MemQLEngine{concepts: memoryNodes.DefaultRegistry()}
}

func connectorCtx(name string) context.Context {
	return auth.ContextWithConnectorActor(context.Background(), name)
}

// The tree's own declaration, read back through the registry. If this
// fails the rest of the file is measuring nothing, which is why it is
// asserted rather than assumed.
func TestTheShippedMirrorIsDeclaredAsOne(t *testing.T) {
	testEngineOverLoadedTree(t)
	c, err := memoryNodes.Get(testMirrorConcept)
	if err != nil || c == nil {
		t.Fatalf("Get(%s): %v", testMirrorConcept, err)
	}
	if !c.IsMirror() {
		t.Fatalf("%s DataState = %q, want mirror -- the mirror guard has nothing to guard otherwise",
			testMirrorConcept, c.DataState())
	}
	if got := c.EffectiveOrigin(); got != "shopify" {
		t.Errorf("EffectiveOrigin() = %q, want \"shopify\"", got)
	}
}

// D3: a mirror is read-only by construction. Every actor but the
// connector its origin names is refused -- and the two escapes the
// row-authz write guard grants are DELIBERATELY not granted here.
func TestMirrorWritesAreRefusedForEveryActorButItsConnector(t *testing.T) {
	e := testEngineOverLoadedTree(t)

	cases := []struct {
		name string
		ctx  context.Context
	}{
		{"an ordinary user", callerCtx("u1")},
		{"a caller with no identity at all", context.Background()},
		{"a CLUSTER OWNER -- an operator's edit is reverted by the next reconcile exactly like anyone else's", ownerRoleCtx("owner1")},
		{"trusted internal server-side Go -- it says the ENGINE is writing, not that SHOPIFY is", auth.ContextWithInternalOrigin(context.Background())},
		{"a DIFFERENT connector", connectorCtx("quickBooks")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := e.guardMirrorWrite(tc.ctx, testMirrorConcept)
			if err == nil {
				t.Fatalf("write to %s was ADMITTED for %s -- a mirror a user can edit is a row MemQL believes and Shopify has never heard of",
					testMirrorConcept, tc.name)
			}
			if !IsMirrorWriteRefused(err) {
				t.Errorf("refusal is not typed as a mirror refusal: %v", err)
			}
			for _, want := range []string{AuditActionMirrorWriteRefused, "shopify"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not name %q -- the only useful thing to tell a caller is WHERE the change has to be made", err, want)
				}
			}
		})
	}
}

func TestTheNamedConnectorMayWriteItsOwnMirror(t *testing.T) {
	e := testEngineOverLoadedTree(t)
	if err := e.guardMirrorWrite(connectorCtx("shopify"), testMirrorConcept); err != nil {
		t.Fatalf("the shopify connector was refused its own mirror: %v", err)
	}
}

// Native and origin concepts are written normally; the guard is inert
// for them. Measured against a real native concept so "the guard is
// off" is not being confused with "no concept reached it".
func TestTheMirrorGuardIsInertForConceptsThatAreNotMirrors(t *testing.T) {
	e := testEngineOverLoadedTree(t)
	native := 0
	for _, c := range memoryNodes.List() {
		if c == nil || c.IsMirror() {
			continue
		}
		native++
		if err := e.guardMirrorWrite(callerCtx("u1"), c.Name); err != nil {
			t.Fatalf("guardMirrorWrite refused non-mirror concept %s (state %q): %v", c.Name, c.DataState(), err)
		}
	}
	if native == 0 {
		t.Fatal("no non-mirror concepts were examined -- this test proved nothing")
	}
	t.Logf("%d non-mirror concepts admitted", native)
}

// D4: a connector actor reads the concepts that name it, and nothing
// else -- whatever tier those other concepts declare, including none.
func TestConnectorRowAdmissionIsScopedToTheConceptsThatNameIt(t *testing.T) {
	testEngineOverLoadedTree(t)
	shopify := connectorCtx("shopify")

	if got := rowAuthzAdmits(shopify, testMirrorConcept, "gid://shopify/Product/1", []byte(`{}`)); got != rowAuthzAdmit {
		t.Errorf("the shopify connector was DENIED %s, the mirror its own @origin names (admission=%d)", testMirrorConcept, got)
	}

	// A clusterOwner-tier concept it does not name. The connector is not
	// a cluster owner, so the tier would deny it anyway -- which is why
	// the interesting case is the next one.
	if got := rowAuthzAdmits(shopify, "v1:campaigns:sendJob", "job1", []byte(`{}`)); got != rowAuthzDeny {
		t.Errorf("the shopify connector reached v1:campaigns:sendJob (admission=%d)", got)
	}

	// AN UNDECLARED CONCEPT. This is the case that makes the rule
	// targeted rather than a bypass: an undeclared concept admits
	// everyone, so a connector falling through to the ordinary tiers
	// would inherit that -- and ~88 of the tree's concepts are
	// undeclared.
	undeclared := firstUndeclaredNonMirrorConcept(t)
	if got := rowAuthzAdmits(shopify, undeclared, "x", []byte(`{}`)); got != rowAuthzDeny {
		t.Errorf("the shopify connector reached %s, an UNDECLARED concept that does not name it (admission=%d). "+
			"Falling through to the tier would hand a connector everything the tree has not gated",
			undeclared, got)
	}

	// A second connector gets the mirror image of the same answer.
	if got := rowAuthzAdmits(connectorCtx("quickBooks"), testMirrorConcept, "p1", []byte(`{}`)); got != rowAuthzDeny {
		t.Errorf("the quickBooks connector reached shopify's mirror (admission=%d)", got)
	}
}

// firstUndeclaredNonMirrorConcept finds a concept declaring no row-authz
// tier -- the population that admits everyone.
func firstUndeclaredNonMirrorConcept(t *testing.T) string {
	t.Helper()
	best := ""
	for _, c := range memoryNodes.List() {
		if c == nil || c.RowAuthz != nil || c.IsMirror() {
			continue
		}
		if best == "" || c.Name < best {
			best = c.Name
		}
	}
	if best == "" {
		t.Skip("no undeclared concept in the tree; the fall-through case cannot be measured here")
	}
	return best
}

// An ordinary caller is completely unaffected by the connector branch:
// it must answer "not a connector" and fall through to the tier.
func TestOrdinaryCallersFallThroughTheConnectorBranch(t *testing.T) {
	testEngineOverLoadedTree(t)
	if got := rowAuthzAdmits(ownerRoleCtx("owner1"), testMirrorConcept, "p1", []byte(`{}`)); got != rowAuthzAdmit {
		t.Errorf("a cluster owner was denied a clusterOwner-tier mirror (admission=%d) -- reading a mirror is not writing one", got)
	}
	if got := rowAuthzAdmits(callerCtx("u1"), testMirrorConcept, "p1", []byte(`{}`)); got != rowAuthzDeny {
		t.Errorf("an ordinary user reached a clusterOwner-tier concept (admission=%d)", got)
	}
}

// The tier predicate enforceRowAuthzOnPlan injects would otherwise
// return ZERO ROWS to the connector that maintains the mirror -- an
// empty result, not an error, indistinguishable from "the mirror is
// empty".
func TestTheInjectedTierIsRelaxedForTheConnectorThatOwnsTheConcept(t *testing.T) {
	testEngineOverLoadedTree(t)

	build := func(t *testing.T) *QueryPlan {
		t.Helper()
		plan := &QueryPlan{
			BoundConcept: testMirrorConcept,
			Root:         &ComparisonExpression{Field: FieldReference{Raw: "present", Parts: []string{"present"}}, Operator: OpEq, Value: true},
		}
		if err := enforceRowAuthzOnPlan(plan); err != nil {
			t.Fatalf("enforceRowAuthzOnPlan: %v", err)
		}
		if !plan.RowAuthzInjected {
			t.Fatalf("the tier was not injected, so there is nothing to relax -- this test would pass vacuously")
		}
		return plan
	}

	t.Run("the named connector", func(t *testing.T) {
		plan := build(t)
		before := canonicalExpression(plan.Root)
		relaxRowAuthzForConnector(connectorCtx("shopify"), plan)
		if !plan.RowAuthzRelaxedForConnector {
			t.Fatal("the connector's plan was not relaxed; its reconcile read would return nothing")
		}
		if after := canonicalExpression(plan.Root); after == before {
			t.Errorf("the plan root is unchanged (%s) -- the injected term is still there", after)
		}
	})

	t.Run("a different connector keeps the tier", func(t *testing.T) {
		plan := build(t)
		relaxRowAuthzForConnector(connectorCtx("quickBooks"), plan)
		if plan.RowAuthzRelaxedForConnector {
			t.Error("a connector the concept does not name had the tier relaxed for it")
		}
	})

	t.Run("an ordinary caller keeps the tier", func(t *testing.T) {
		plan := build(t)
		relaxRowAuthzForConnector(callerCtx("u1"), plan)
		if plan.RowAuthzRelaxedForConnector {
			t.Error("an ordinary caller had the tier relaxed for them")
		}
	})
}

// A relaxed plan's canonical expression is byte-identical to what an
// ordinary caller writes over the same concept, so the caller identity
// MUST stay in the cache signature or the connector's tier-free result
// is served to whoever asks next (the memql#4040 collision shape).
func TestARelaxedPlanStillFoldsTheCallerIntoTheCacheKey(t *testing.T) {
	e := testEngineOverLoadedTree(t)
	plan := &QueryPlan{
		BoundConcept: testMirrorConcept,
		Root:         &ComparisonExpression{Field: FieldReference{Raw: "present", Parts: []string{"present"}}, Operator: OpEq, Value: true},
	}
	if err := enforceRowAuthzOnPlan(plan); err != nil {
		t.Fatalf("enforceRowAuthzOnPlan: %v", err)
	}
	ctx := connectorCtx("shopify")
	relaxRowAuthzForConnector(ctx, plan)

	plain := &QueryPlan{
		BoundConcept: testMirrorConcept,
		Root:         &ComparisonExpression{Field: FieldReference{Raw: "present", Parts: []string{"present"}}, Operator: OpEq, Value: true},
	}
	if canonicalExpression(plan.Root) != canonicalExpression(plain.Root) {
		t.Skip("the relaxed root is not identical to a plain one here; the collision this guards is not reachable in this shape")
	}
	relaxed := e.planCacheSignature(ctx, plan)
	other := e.planCacheSignature(callerCtx("u1"), plain)
	if relaxed == other {
		t.Fatalf("a relaxed connector plan and an ordinary caller's plan share the cache signature %q -- "+
			"the connector's tier-free result would be served to the next caller", relaxed)
	}
}

// The virtual read: one row per concept, describing what the registry
// declares. Never persisted.
func TestDataOriginsProjectsEveryConceptFromTheLiveRegistry(t *testing.T) {
	e := testEngineOverLoadedTree(t)
	nodes, err := e.evaluateDataOriginsExpression(context.Background())
	if err != nil {
		t.Fatalf("evaluateDataOriginsExpression: %v", err)
	}
	registered := len(memoryNodes.List())
	if len(nodes) != registered {
		t.Fatalf("dataOrigins produced %d rows for %d registered concepts -- one row per concept is the contract",
			len(nodes), registered)
	}

	byId := make(map[string]map[string]any, len(nodes))
	for _, n := range nodes {
		var payload map[string]any
		if err := json.Unmarshal(n.Payload, &payload); err != nil {
			t.Fatalf("row %s payload: %v", n.ID, err)
		}
		byId[n.ID] = payload
		if got, _ := payload["origin"].(string); strings.TrimSpace(got) == "" {
			t.Fatalf("row %s reports an empty origin -- a client would have to re-derive the default", n.ID)
		}
	}

	mirror, ok := byId[testMirrorConcept]
	if !ok {
		t.Fatalf("dataOrigins has no row for %s", testMirrorConcept)
	}
	if got, _ := mirror["dataState"].(string); got != "mirror" {
		t.Errorf("%s dataState = %q, want \"mirror\"", testMirrorConcept, got)
	}
	if got, _ := mirror["origin"].(string); got != "shopify" {
		t.Errorf("%s origin = %q, want \"shopify\"", testMirrorConcept, got)
	}
	connectors, _ := mirror["connectors"].([]any)
	if len(connectors) != 1 || connectors[0] != "shopify" {
		t.Errorf("%s connectors = %v, want [shopify]", testMirrorConcept, connectors)
	}
}
