package memql_test

// authoring_runtime_test.go -- coverage for the owner-scoped authored-construct
// runtime registry + the core-first resolver (epic memql#954, issue #959,
// increment 1).
//
// Two layers under test:
//   - AuthoredRuntimeRegistry: thread-safe, owner-scoped register / resolve /
//     pause / resume / retire / version, mutable after engine startup.
//   - ResolveConstruct + EngineCoreHas: core-first precedence -- an authored
//     construct can never shadow a sealed core one.
//
// The resolver-precedence test runs against a FULLY initialized engine
// (TestEngineInitLoadsFullDSL style) so EngineCoreHas is exercised against the
// real sealed core registries, not a stub.

import (
	"sync"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

func newAuthored(owner, kind, name string, version int) *memql.AuthoredConstruct {
	return &memql.AuthoredConstruct{
		OwnerUserId: owner,
		Kind:        kind,
		Name:        name,
		BundleId:    "v1:authoring:bundle:test",
		Version:     version,
		Source:      "// " + name,
	}
}

// TestAuthoredRegistry_RegisterAndResolve: a registered construct resolves
// active for its owner.
func TestAuthoredRegistry_RegisterAndResolve(t *testing.T) {
	r := memql.NewAuthoredRuntimeRegistry()
	if err := r.Register(newAuthored("user-a", "automation", "draftReply", 1)); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, ok := r.Resolve("user-a", "automation", "draftReply")
	if !ok {
		t.Fatal("expected to resolve a registered active construct")
	}
	if got.Status != memql.AuthoredActive || got.Version != 1 {
		t.Errorf("unexpected entry: %+v", got)
	}
}

// TestAuthoredRegistry_OwnerScoped: two owners can register the same
// (kind, name) without collision, and one owner can never resolve the other's.
func TestAuthoredRegistry_OwnerScoped(t *testing.T) {
	r := memql.NewAuthoredRuntimeRegistry()
	if err := r.Register(newAuthored("user-a", "automation", "shared", 1)); err != nil {
		t.Fatalf("register a: %v", err)
	}
	if err := r.Register(newAuthored("user-b", "automation", "shared", 1)); err != nil {
		t.Fatalf("register b (same kind+name, different owner): %v", err)
	}
	if _, ok := r.Resolve("user-a", "automation", "shared"); !ok {
		t.Error("owner a should resolve its own construct")
	}
	// Owner isolation: user-a must not see user-c (unregistered) -- and the
	// ListForOwner enumeration must be owner-private.
	if got := r.ListForOwner("user-a"); len(got) != 1 || got[0].OwnerUserId != "user-a" {
		t.Errorf("ListForOwner leaked across owners: %+v", got)
	}
	if r.Count() != 2 {
		t.Errorf("expected 2 total constructs, got %d", r.Count())
	}
}

// TestAuthoredRegistry_PauseResumeRetire: live lifecycle transitions.
func TestAuthoredRegistry_PauseResumeRetire(t *testing.T) {
	r := memql.NewAuthoredRuntimeRegistry()
	if err := r.Register(newAuthored("user-a", "automation", "x", 1)); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Pause -> stops resolving, but Lookup still sees it.
	if err := r.Pause("user-a", "automation", "x"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, ok := r.Resolve("user-a", "automation", "x"); ok {
		t.Error("paused construct must not resolve")
	}
	if entry, ok := r.Lookup("user-a", "automation", "x"); !ok || entry.Status != memql.AuthoredPaused {
		t.Error("paused construct must still be visible via Lookup")
	}
	// Pause is idempotent (the circuit breaker relies on it).
	if err := r.Pause("user-a", "automation", "x"); err != nil {
		t.Errorf("double pause should be a no-op, got: %v", err)
	}

	// Resume -> resolves again.
	if err := r.Resume("user-a", "automation", "x"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, ok := r.Resolve("user-a", "automation", "x"); !ok {
		t.Error("resumed construct must resolve")
	}

	// Retire -> gone entirely.
	if err := r.Retire("user-a", "automation", "x"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, ok := r.Lookup("user-a", "automation", "x"); ok {
		t.Error("retired construct must be removed")
	}
	if r.Count() != 0 {
		t.Errorf("expected empty registry after retire, got %d", r.Count())
	}
}

// TestAuthoredRegistry_Versioning: re-registering replaces in place only when
// the version strictly increases; a stale version is rejected.
func TestAuthoredRegistry_Versioning(t *testing.T) {
	r := memql.NewAuthoredRuntimeRegistry()
	if err := r.Register(newAuthored("user-a", "automation", "x", 2)); err != nil {
		t.Fatalf("register v2: %v", err)
	}

	// A newer version supersedes in place.
	if err := r.Register(newAuthored("user-a", "automation", "x", 3)); err != nil {
		t.Fatalf("register v3: %v", err)
	}
	got, _ := r.Resolve("user-a", "automation", "x")
	if got.Version != 3 {
		t.Errorf("expected v3 after supersede, got v%d", got.Version)
	}
	if r.Count() != 1 {
		t.Errorf("supersede should replace in place, count=%d", r.Count())
	}

	// A stale (<=) version is rejected.
	if err := r.Register(newAuthored("user-a", "automation", "x", 3)); err == nil {
		t.Error("expected re-register of same version to be rejected")
	}
	if err := r.Register(newAuthored("user-a", "automation", "x", 1)); err == nil {
		t.Error("expected downgrade to be rejected")
	}
}

// TestAuthoredRegistry_RejectsBadInput: nil / empty-owner / empty-name.
func TestAuthoredRegistry_RejectsBadInput(t *testing.T) {
	r := memql.NewAuthoredRuntimeRegistry()
	if err := r.Register(nil); err == nil {
		t.Error("nil construct should error")
	}
	if err := r.Register(newAuthored("", "automation", "x", 1)); err == nil {
		t.Error("empty owner should error")
	}
	if err := r.Register(newAuthored("user-a", "automation", "", 1)); err == nil {
		t.Error("empty name should error")
	}
}

// TestAuthoredRegistry_Concurrent: race-detector smoke for the thread-safe
// register/resolve path (mirrors IntegrationRegistry's concurrency contract).
func TestAuthoredRegistry_Concurrent(t *testing.T) {
	r := memql.NewAuthoredRuntimeRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			owner := "user-a"
			name := "auto"
			_ = r.Register(newAuthored(owner, "automation", name, n+1))
			_, _ = r.Resolve(owner, "automation", name)
			_ = r.ListForOwner(owner)
		}(i)
	}
	wg.Wait()
	if _, ok := r.Lookup("user-a", "automation", "auto"); !ok {
		t.Error("expected the construct to survive concurrent registration")
	}
}

// --- resolver precedence (core-first) ---

// stubCore is a deterministic CoreHasFunc for the precedence unit tests.
func stubCore(names map[string]bool) memql.CoreHasFunc {
	return func(kind, name string) bool { return names[kind+"/"+name] }
}

// TestResolveConstruct_CoreShadowsAuthored: when core defines a name, the
// authored layer is NEVER consulted -- even if the owner registered the same
// name. This is the one-way precedence guarantee.
func TestResolveConstruct_CoreShadowsAuthored(t *testing.T) {
	authored := memql.NewAuthoredRuntimeRegistry()
	if err := authored.Register(newAuthored("user-a", "query", "spaceParticipants", 1)); err != nil {
		t.Fatalf("register: %v", err)
	}
	core := stubCore(map[string]bool{"query/spaceParticipants": true})

	res := memql.ResolveConstruct(core, authored, "user-a", "query", "spaceParticipants")
	if res.Source != "core" {
		t.Fatalf("core must win over an authored construct of the same name, got %q", res.Source)
	}
	if res.Authored != nil {
		t.Error("a core hit must not carry an authored entry")
	}
}

// TestResolveConstruct_AuthoredFillsCoreGap: a name core does not define
// resolves from the owner's authored layer.
func TestResolveConstruct_AuthoredFillsCoreGap(t *testing.T) {
	authored := memql.NewAuthoredRuntimeRegistry()
	_ = authored.Register(newAuthored("user-a", "automation", "ownerOnly", 1))
	core := stubCore(nil) // core defines nothing

	res := memql.ResolveConstruct(core, authored, "user-a", "automation", "ownerOnly")
	if res.Source != "authored" || res.Authored == nil {
		t.Fatalf("expected authored fill-in, got %+v", res)
	}

	// A different owner gets nothing (owner-scoped).
	if other := memql.ResolveConstruct(core, authored, "user-b", "automation", "ownerOnly"); other.Source != "" {
		t.Errorf("authored layer leaked across owners: %+v", other)
	}

	// A paused authored construct does not resolve.
	_ = authored.Pause("user-a", "automation", "ownerOnly")
	if paused := memql.ResolveConstruct(core, authored, "user-a", "automation", "ownerOnly"); paused.Source != "" {
		t.Errorf("paused authored construct must not resolve, got %+v", paused)
	}
}

// TestResolveConstruct_EngineCoreHas runs the resolver against a fully
// initialized engine so EngineCoreHas is exercised against the REAL sealed
// core registries -- mirroring the engine-init smoke test. A real core query
// name must report core; an owner's authored construct of that same name must
// be shadowed; an owner-only name must fall through to authored.
func TestResolveConstruct_EngineCoreHas(t *testing.T) {
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concept.DefaultRegistry()
	eng, err := memql.New(nil)
	if err != nil {
		t.Fatalf("construct engine: %v", err)
	}
	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine.Init: %v", err)
	}

	core := memql.EngineCoreHas(eng)
	if !core("query", "spaceParticipants") {
		t.Fatal("EngineCoreHas should report a real core query")
	}
	if core("automation", "definitelyNotACoreConstructXyz") {
		t.Error("EngineCoreHas should report false for a non-core name")
	}

	authored := memql.NewAuthoredRuntimeRegistry()
	// Owner authors a construct colliding with a core query name + an
	// owner-only automation name.
	_ = authored.Register(newAuthored("user-a", "query", "spaceParticipants", 1))
	_ = authored.Register(newAuthored("user-a", "automation", "myCustomSweep", 1))

	if res := memql.ResolveConstruct(core, authored, "user-a", "query", "spaceParticipants"); res.Source != "core" {
		t.Errorf("core query must shadow the authored collision, got %q", res.Source)
	}
	if res := memql.ResolveConstruct(core, authored, "user-a", "automation", "myCustomSweep"); res.Source != "authored" {
		t.Errorf("owner-only authored construct must resolve from the authored layer, got %q", res.Source)
	}
}
