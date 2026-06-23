package auth

import (
	"sync"
	"testing"
)

// TestCapableIsTheCanonicalPrimitive pins that the migrated Can* adapters are
// exactly the Capable() decisions they claim to be -- i.e. the single
// enforcement primitive (Capable) and the convenience adapters can never drift
// apart. This is the E1.6 "consistent server-side" guard at the API level: one
// definition of "may role R do verb V on resource T".
func TestCapableIsTheCanonicalPrimitive(t *testing.T) {
	for _, slug := range allSlugs {
		u := UserContext{ID: "u", Role: slug}
		pairs := []struct {
			adapter bool
			capable bool
			name    string
		}{
			{AtLeastAdmin(u), Capable(slug, VerbCreate, ResourcePrincipal), "AtLeastAdmin == create/principal"},
			{IsPrivilegedUser(u), Capable(slug, VerbCreate, ResourcePrincipal), "IsPrivilegedUser == create/principal"},
			{AtLeastDeveloper(u), Capable(slug, VerbExecute, ResourceDeployment), "AtLeastDeveloper == execute/deployment"},
			{CanWrite(u), Capable(slug, VerbCreate, ResourceData), "CanWrite == create/data"},
			{CanAuthor(u), Capable(slug, VerbCreate, ResourceConstruct), "CanAuthor == create/construct"},
			{CanRunInline(u), Capable(slug, VerbExecute, ResourceConstruct), "CanRunInline == execute/construct"},
			{CanRead(u), Capable(slug, VerbRead, ResourceData), "CanRead == read/data"},
			{CanCreateAgent(u), Capable(slug, VerbCreate, ResourceAgent), "CanCreateAgent == create/agent"},
			{CanManageGroup(u), Capable(slug, VerbCreate, ResourceGroup), "CanManageGroup == create/group"},
		}
		for _, p := range pairs {
			if p.adapter != p.capable {
				t.Errorf("slug %q: %s -- adapter=%v capable=%v (the Can* adapter drifted from Capable)", slug, p.name, p.adapter, p.capable)
			}
		}
	}
}

// TestCapableNodeConsistency is the E1.6 multi-node acceptance at the unit
// level: an authorization decision must be identical regardless of WHICH node
// resolves it. The consolidated model carries no per-node / per-instance state
// -- Capable is a pure lookup over a static capability set -- so the same
// (role, verb, resource) ALWAYS yields the same decision. This test simulates
// many concurrent "nodes" resolving the same decisions in parallel and asserts
// every node agrees with a single reference resolution. A regression that
// introduced node-local state (a mutable cache, an env-dependent branch) would
// surface here as a divergence or a race.
func TestCapableNodeConsistency(t *testing.T) {
	type query struct {
		role     Role
		verb     string
		resource string
	}
	queries := []query{}
	verbs := []string{VerbRead, VerbCreate, VerbUpdate, VerbDelete, VerbExecute}
	resources := []string{ResourcePrincipal, ResourceConstruct, ResourceData, ResourceDeployment, ResourceAgent, ResourceGroup, ResourceRole}
	for _, slug := range allSlugs {
		for _, v := range verbs {
			for _, r := range resources {
				queries = append(queries, query{slug, v, r})
			}
		}
	}

	// Reference resolution (one "node").
	want := make([]bool, len(queries))
	for i, q := range queries {
		want[i] = Capable(q.role, q.verb, q.resource)
	}

	// N concurrent "nodes" must all agree with the reference.
	const nodes = 16
	var wg sync.WaitGroup
	mismatch := make(chan string, nodes*len(queries))
	for n := 0; n < nodes; n++ {
		wg.Add(1)
		go func(node int) {
			defer wg.Done()
			for i, q := range queries {
				if got := Capable(q.role, q.verb, q.resource); got != want[i] {
					mismatch <- "node decision diverged for a (role,verb,resource)"
				}
			}
		}(n)
	}
	wg.Wait()
	close(mismatch)
	if len(mismatch) > 0 {
		t.Fatalf("Capable decisions diverged across %d concurrent nodes -- authorization is NOT node-consistent (per-node state leaked into the model)", len(mismatch))
	}
}

// TestGovernPrincipalNodeConsistency mirrors the above for the relational
// governance primitive: GovernPrincipal is pure arithmetic over the passed
// principals, so it too is node-agnostic. The enforcement path resolves the
// ranks from the (node-agnostic) catalog and hands them here, so a consistent
// catalog yields a consistent decision on every replica.
func TestGovernPrincipalNodeConsistency(t *testing.T) {
	ranks := []int{rankViewer, rankUser, rankAdmin, rankDeveloper, rankOwner}
	verbs := []GovernVerb{GovernRead, GovernCreate, GovernUpdate, GovernDelete}
	for _, ar := range ranks {
		for _, tr := range ranks {
			for _, v := range verbs {
				actor := Principal{UserId: "a", Rank: ar, IsOwner: ar == rankOwner}
				target := Principal{UserId: "b", Rank: tr, IsOwner: tr == rankOwner}
				ref := GovernPrincipal(actor, target, v)
				for n := 0; n < 8; n++ {
					if got := GovernPrincipal(actor, target, v); got != ref {
						t.Fatalf("GovernPrincipal diverged on repeat resolution (actorRank=%d targetRank=%d verb=%s): %v != %v -- not deterministic/node-consistent", ar, tr, v, got, ref)
					}
				}
			}
		}
	}
}
