package memql

import (
	"sort"
	"testing"
)

// agents_owner_registry_test.go -- memql#3216.
//
// The registry was map[roleSlug] -> *AgentDefinition, and roleSlug is
// caller-supplied: createAgent takes it as a plain arg with no enum, no
// catalog validation and no uniqueness check, and updateAgent splats a
// free-form payload onto any agentId with no ownership predicate. So a slug
// names a BUCKET, not an agent -- and askSpecialist resolved one with a bare
// map lookup, then fed def.Name / def.Description / def.SystemPrompt into the
// specialist prompt. Under a contested slug, one user's assistant could be
// handed another user's specialist persona verbatim.
//
// memql#3209 made that contest ORDER-INDEPENDENT, which removed the hazard of
// an unrelated edit changing who answers. It did not make the winner the RIGHT
// agent for the asking user, because nothing in the key said who was asking.
//
// # Every test here carries a POSITIVE CONTROL, deliberately
//
// The issue asked for it by name, and the reason is specific: the registry
// spent memql#3209 EMPTY (extractRowList had no *ExecuteResult arm), so
// "user B does not resolve user A's specialist" was satisfiable by a map with
// nothing in it. A miss-only assertion would have passed against the defect,
// against the fix, and against a registry that was simply broken. So each test
// asserts the OWNER HITS the expected row id first, and only then that the
// other user misses.

func ownedDef(t *testing.T, owner, id, roleSlug, name, kind string) *AgentDefinition {
	t.Helper()
	def, ok := agentDefinitionFromRow(ownedRowFor(owner, id, roleSlug, name, kind))
	if !ok {
		t.Fatalf("agentDefinitionFromRow rejected the fixture row %s", id)
	}
	return def
}

const (
	alice = "v1:identity:user:alice"
	bob   = "v1:identity:user:bob"
)

// THE HEADLINE. Two users each hold a row under "human-resources" -- which
// costs nothing to arrange, since roleSlug is caller-chosen. Each must resolve
// their OWN.
func TestOwnerKeyedRegistryResolvesPerOwner(t *testing.T) {
	r := NewAgentRegistry()
	for _, def := range []*AgentDefinition{
		ownedDef(t, alice, "v1:agents:agent:a1", "human-resources", "Alice HR", "specialist"),
		ownedDef(t, bob, "v1:agents:agent:b1", "human-resources", "Bob HR", "specialist"),
	} {
		for key, resolved := range buildAgentIndex([]*AgentDefinition{def})[def.OwnerUserId] {
			_ = key
			if err := r.Upsert(resolved); err != nil {
				t.Fatalf("Upsert: %v", err)
			}
		}
	}

	// POSITIVE CONTROL, both directions. Without these the misses below are
	// satisfied by an empty registry -- which is exactly the state this code
	// was in while the defect was unreachable.
	got, ok := r.Get(alice, "human-resources")
	if !ok || got == nil {
		t.Fatal("alice cannot resolve her OWN specialist -- the negative assertions below would " +
			"pass vacuously, which is how a miss-only test passes against an empty registry")
	}
	if got.Id != "v1:agents:agent:a1" {
		t.Errorf("alice resolved %q, want her own row a1 -- she was handed %q's persona",
			got.Id, got.OwnerUserId)
	}
	got, ok = r.Get(bob, "human-resources")
	if !ok || got == nil {
		t.Fatal("bob cannot resolve his OWN specialist")
	}
	if got.Id != "v1:agents:agent:b1" {
		t.Errorf("bob resolved %q, want his own row b1", got.Id)
	}
}

// Neither user may reach the other's row when only ONE of them holds the slug.
// This is the shape the miss actually takes in production: a user asks for a
// specialist they do not have, and the cluster contains someone else's.
func TestOwnerKeyedRegistryDoesNotFallThroughToAnotherOwner(t *testing.T) {
	r := NewAgentRegistry()
	def := ownedDef(t, alice, "v1:agents:agent:a1", "human-resources", "Alice HR", "specialist")
	def.Name = "human-resources"
	if err := r.Upsert(def); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// POSITIVE CONTROL.
	if got, ok := r.Get(alice, "human-resources"); !ok || got == nil || got.Id != "v1:agents:agent:a1" {
		t.Fatalf("the owning user does not resolve her own row (%v, %v) -- the assertion below "+
			"would then pass against an empty registry", got, ok)
	}

	if got, ok := r.Get(bob, "human-resources"); ok || got != nil {
		t.Errorf("bob resolved %q (owner %q) for a slug only alice holds.\n\n"+
			"askSpecialist feeds the resolved def's Description and SystemPrompt into a prompt, "+
			"so this is one user's assistant being handed another user's specialist persona "+
			"verbatim. memql#3216.", got.Id, got.OwnerUserId)
	}
}

// THE DOCUMENTED FALLBACK. A row with no ownerUserId is the shared catalog:
// every user resolves it, and it is what an unowned platform row IS.
func TestOwnerKeyedRegistryFallsBackToTheSharedCatalog(t *testing.T) {
	r := NewAgentRegistry()
	shared := ownedDef(t, "", "v1:agents:agent:sys", "system-planner", "MemQL Planner", agentKindSystem)
	shared.Name = "system-planner"
	if err := r.Upsert(shared); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	for _, who := range []string{alice, bob, ""} {
		got, ok := r.Get(who, "system-planner")
		if !ok || got == nil || got.Id != "v1:agents:agent:sys" {
			t.Errorf("owner %q cannot resolve the shared-catalog row (%v, %v).\n\n"+
				"A row with no ownerUserId is unowned, not unknown -- a user with no agent of "+
				"their own must still reach the platform ones.", who, got, ok)
		}
	}
}

// The owner's own row must WIN over a shared one under the same key: the
// fallback is a fallback, not a merge. Otherwise the shared catalog would be a
// way to displace a user's own specialist by claiming its slug unowned.
func TestOwnerKeyedRegistryPrefersTheOwnersOwnRowOverTheSharedOne(t *testing.T) {
	r := NewAgentRegistry()
	shared := ownedDef(t, "", "v1:agents:agent:shared", "human-resources", "Stock HR", "specialist")
	shared.Name = "human-resources"
	mine := ownedDef(t, alice, "v1:agents:agent:a1", "human-resources", "Alice HR", "specialist")
	mine.Name = "human-resources"
	for _, d := range []*AgentDefinition{shared, mine} {
		if err := r.Upsert(d); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	got, ok := r.Get(alice, "human-resources")
	if !ok || got == nil {
		t.Fatal("alice resolves nothing at all")
	}
	if got.Id != "v1:agents:agent:a1" {
		t.Errorf("alice resolved %q, want her own row -- the shared catalog is a FALLBACK, and a "+
			"shared row displacing an owned one would make claiming a slug unowned a way to "+
			"replace someone's specialist", got.Id)
	}
	// And bob, who owns nothing, still gets the shared one.
	if got, ok := r.Get(bob, "human-resources"); !ok || got.Id != "v1:agents:agent:shared" {
		t.Errorf("bob resolved %v, want the shared row", got)
	}
}

// An EMPTY owner must resolve the shared catalog and nothing else. It is not a
// wildcard: a caller that could not establish who is asking must not be handed
// the first matching persona in the cluster.
func TestAnEmptyOwnerIsNotAWildcard(t *testing.T) {
	r := NewAgentRegistry()
	mine := ownedDef(t, alice, "v1:agents:agent:a1", "human-resources", "Alice HR", "specialist")
	mine.Name = "human-resources"
	if err := r.Upsert(mine); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// POSITIVE CONTROL: the registry is not empty.
	if _, ok := r.Get(alice, "human-resources"); !ok {
		t.Fatal("the registry holds nothing, so the assertion below proves nothing")
	}

	if got, ok := r.Get("", "human-resources"); ok || got != nil {
		t.Errorf("an empty owner resolved %q, which belongs to %q.\n\n"+
			"Empty means 'this call cannot say who is asking', and the answer to that is the "+
			"shared catalog or nothing -- never the first bucket that happens to hold the slug.",
			got.Id, got.OwnerUserId)
	}
}

// buildAgentIndex must contest keys WITHIN an owner, not across owners. Two
// users holding the same slug is not a contest and neither may lose.
func TestBuildAgentIndexContestsWithinAnOwnerOnly(t *testing.T) {
	// Bob's row has the LOWER id, so a global contest -- whose final tie-break
	// is lowest row id -- would hand alice's key to bob.
	defs := []*AgentDefinition{
		ownedDef(t, alice, "v1:agents:agent:zzz", "human-resources", "Alice HR", "specialist"),
		ownedDef(t, bob, "v1:agents:agent:aaa", "human-resources", "Bob HR", "specialist"),
	}

	idx := buildAgentIndex(defs)
	if len(idx) != 2 {
		t.Fatalf("index has %d owner buckets, want 2 -- two users holding one slug is not a "+
			"contest", len(idx))
	}
	if got := idx[alice]["human-resources"]; got == nil || got.Id != "v1:agents:agent:zzz" {
		t.Errorf("alice's bucket resolved %v, want her own row. A global contest would have "+
			"given her key to bob, whose row id sorts lower.", got)
	}
	if got := idx[bob]["human-resources"]; got == nil || got.Id != "v1:agents:agent:aaa" {
		t.Errorf("bob's bucket resolved %v, want his own row", got)
	}
}

// Within one owner the memql#3209 comparator still decides, unchanged. The
// owner dimension partitions the contest; it does not replace it.
func TestBuildAgentIndexStillResolvesAContestWithinOneOwner(t *testing.T) {
	defs := []*AgentDefinition{
		ownedDef(t, alice, "v1:agents:agent:zzz", "human-resources", "HR Two", "specialist"),
		ownedDef(t, alice, "v1:agents:agent:aaa", "human-resources", "HR One", "specialist"),
	}
	for i, perm := range permutations(defs) {
		idx := buildAgentIndex(perm)
		if got := idx[alice]["human-resources"]; got == nil || got.Id != "v1:agents:agent:aaa" {
			t.Errorf("ordering %d resolved %v, want the lowest row id -- the tie-break must "+
				"still be a property of the rows", i, got)
		}
	}
}

// NamesFor must not answer "which specialists do other users have". Its one
// production caller puts the result in an error message that reaches an LLM.
func TestNamesForDoesNotLeakAnotherOwnersSlugs(t *testing.T) {
	r := NewAgentRegistry()
	for _, d := range []*AgentDefinition{
		func() *AgentDefinition {
			d := ownedDef(t, alice, "v1:agents:agent:a1", "alice-only", "Alice HR", "specialist")
			d.Name = "alice-only"
			return d
		}(),
		func() *AgentDefinition {
			d := ownedDef(t, bob, "v1:agents:agent:b1", "bob-only", "Bob HR", "specialist")
			d.Name = "bob-only"
			return d
		}(),
		func() *AgentDefinition {
			d := ownedDef(t, "", "v1:agents:agent:sys", "shared-one", "Shared", agentKindSystem)
			d.Name = "shared-one"
			return d
		}(),
	} {
		if err := r.Upsert(d); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	got := r.NamesFor(alice)
	sort.Strings(got)
	want := []string{"alice-only", "shared-one"}
	if len(got) != len(want) {
		t.Fatalf("NamesFor(alice) = %v, want exactly %v.\n\n"+
			"This list lands in 'no agent registered with that role (loaded: %%v)', which is "+
			"returned to the model. A global list answers 'what specialists does everyone else "+
			"have' to anyone who can provoke a miss.", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("NamesFor(alice) = %v, want %v", got, want)
			break
		}
	}
}
