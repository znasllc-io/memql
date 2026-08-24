package memql

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// Cross-node re-resolution of AI provider auth (epic memql#4440, design D5).
//
// THE BUG CLASS THIS EXISTS FOR. Provider auth resolves once per process at
// boot. The portal's Apply is one gRPC call and lands on whichever replica the
// front door picked. So a reload that acted only on the node it reached would
// leave a fleet where some replicas can call the vendor and some cannot --
// which presents to a user as an assistant that works on every other message,
// and points every diagnosis at the vendor rather than at us. A green
// single-node test is a FALSE signal for exactly this.

// seededResolver stands in for concept storage. It starts empty -- the state a
// freshly installed cluster is in -- and `seed` is the portal write that
// happens while both nodes are already running with their providers resolved
// (that is, unresolved).
type seededResolver struct {
	mu     sync.Mutex
	values map[string]string
}

func newSeededResolver() *seededResolver {
	return &seededResolver{values: map[string]string{}}
}

func (r *seededResolver) get(_ context.Context, name string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.values[name], nil
}

func (r *seededResolver) seed(name, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[name] = value
}

// installSeededResolver points the package-level resolution chain at a test
// double for the duration of one test, restoring whatever was there.
func installSeededResolver(t *testing.T, r *seededResolver) {
	t.Helper()
	prevSecret, prevVariable := systemSecretResolver, systemVariableResolver
	SetSystemSecretResolver(r.get)
	SetSystemVariableResolver(nil)
	t.Cleanup(func() {
		SetSystemSecretResolver(prevSecret)
		SetSystemVariableResolver(prevVariable)
	})
}

// The owner-only gates are exercised through the package's EXISTING
// clusterOwnerCtx / nonClusterOwnerCtx helpers
// (executor_actor_postfilter_db_test.go), which build REAL AccessContexts.
// That matters more here than the small duplication a local helper would cost:
// per the fake-engine-has-no-gates lesson, a test that fabricated its own
// "is owner" answer would pass against code carrying no gate at all.

// engineWithProviders builds an engine holding one provider whose key comes
// from the shared resolver, in the unresolved state a keyless boot produces.
func engineWithProviders(t *testing.T, bus *events.Bus) *MemQLEngine {
	t.Helper()
	e := &MemQLEngine{specs: newSpecRegistry(), functions: newFunctionRegistry()}
	e.SetEventBus(bus)
	e.providers = newProviderRegistry("")
	return e
}

// TestProvidersReloadPropagation_CrossNode is the load-bearing hop test.
//
// Two SEPARATE engines with their OWN provider registries, bridged by
// forwarding providers.reload.* from A's bus onto B's -- exactly what the mesh
// EventBridge plus the single broadcast routing rule do. A key seeded through
// node A must become usable on node B with no restart.
//
// FAILS against single-node-assuming code: with B's subscriber absent, the
// broadcast is delivered to B's bus and dropped, and B's provider stays
// unavailable forever while A's works. That asymmetry is the bug.
func TestProvidersReloadPropagation_CrossNode(t *testing.T) {
	resolver := newSeededResolver()
	installSeededResolver(t, resolver)

	busA := events.NewBus()
	busB := events.NewBus()
	t.Cleanup(func() { busA.Close(); busB.Close() })

	// Bridge A -> B, stamping an OriginNodeId so it reads as peer-bridged.
	busA.Subscribe(providersReloadPattern, func(ev events.Event) {
		ev.OriginNodeId = "node-A"
		busB.Publish(ev)
	})

	engA := engineWithProviders(t, busA)
	engB := engineWithProviders(t, busB)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// THE propagation under test. Only B subscribes; A reloads synchronously
	// in the builtin, which is what the portal's caller is waiting on.
	engB.StartProvidersReloadSubscriber(ctx)

	// BOTH NODES BOOT KEYLESS, which is the whole premise: the cluster was
	// installed without a provider key. They load the REAL provider tree --
	// the same walk boot does, and the same walk the reload does, so the
	// before and after numbers are comparable.
	for _, e := range []*MemQLEngine{engA, engB} {
		if _, err := LoadUnifiedProviders(nil, e.providers); err != nil {
			t.Fatalf("load providers: %v", err)
		}
	}
	registered := engA.providers.Count()
	if registered == 0 {
		t.Fatal("the provider tree registered nothing; this test would prove nothing")
	}
	if engA.providers.AvailableCount() != 0 || engB.providers.AvailableCount() != 0 {
		t.Fatalf("both nodes must boot with nothing callable (A=%d B=%d)",
			engA.providers.AvailableCount(), engB.providers.AvailableCount())
	}

	// THE PORTAL WRITE. A globalSecret row appears while both nodes are up
	// with their providers already resolved (that is, unresolved).
	resolver.seed("MEMQL_AI_OPENAI_API_KEY", "sk-test-seeded-through-the-portal")
	resolver.seed("MEMQL_AI_ANTHROPIC_API_KEY", "sk-ant-test-seeded-through-the-portal")
	if engA.providers.AvailableCount() != 0 {
		t.Fatal("seeding a row must not by itself change a running node -- auth resolves at boot, " +
			"which is the entire reason this reload seam exists")
	}

	// THE APPLY, on node A only.
	if _, err := engA.evaluateProvidersReloadExpression(clusterOwnerCtx("user-owner"), map[string]any{
		"requestId": "apply-hop-test",
	}); err != nil {
		t.Fatalf("providersReload on engine A: %v", err)
	}

	availableOnA := engA.providers.AvailableCount()
	if availableOnA == 0 {
		t.Fatal("engine A did not pick up the seeded keys")
	}

	// THE HOP. B never handled the Apply and was never restarted.
	if !eventually(5*time.Second, func() bool {
		return engB.providers.AvailableCount() == availableOnA
	}) {
		t.Fatalf("CROSS-NODE FAILURE: engine B has %d callable providers, engine A has %d -- "+
			"the providers.reload broadcast did not propagate, so a key seeded through the portal "+
			"works on whichever replica handled the Apply and on no other. This is the asymmetry "+
			"that presents to a user as an assistant that works on every other message.",
			engB.providers.AvailableCount(), availableOnA)
	}
}

// TestProvidersReloadLoadsTheTree guards the OTHER half of the bug this epic
// found: ReloadAIProviders called loadAIProviders, which returns an EMPTY
// registry by design (its own doc comment says so -- the legacy walk it used
// to do was retired when providers moved to LoadUnifiedProviders, and this
// call site was never updated). So every "reload" replaced the live registry
// with nothing and reported a count of zero.
//
// It had no callers in the tree, which is exactly why that survived. Promoting
// it to the seam behind an operator-facing button makes it load-bearing, so it
// gets a test that fails against the empty-registry version.
func TestProvidersReloadLoadsTheTree(t *testing.T) {
	resolver := newSeededResolver()
	installSeededResolver(t, resolver)

	e := engineWithProviders(t, events.NewBus())
	if _, err := LoadUnifiedProviders(nil, e.providers); err != nil {
		t.Fatalf("load providers: %v", err)
	}
	before := e.providers.Count()
	if before == 0 {
		t.Fatal("nothing registered before the reload; the assertion below would be vacuous")
	}

	if _, err := e.ReloadAIProviders(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if got := e.providers.Count(); got != before {
		t.Errorf("the reload changed the registered count from %d to %d; "+
			"a reload that empties the registry takes every provider away from a running node",
			before, got)
	}
}

func TestProvidersReloadIsOwnerOnly(t *testing.T) {
	// The Go wall, exercised through a REAL AccessContext. A writer can reach
	// the query surface, and a reload rotates credentials fleet-wide.
	e := engineWithProviders(t, events.NewBus())
	_, err := e.evaluateProvidersReloadExpression(nonClusterOwnerCtx("user-writer"), nil)
	if err == nil {
		t.Fatal("a non-owner reloaded every node's provider credentials")
	}
	if !strings.Contains(err.Error(), "owner-only") {
		t.Errorf("the refusal should say why: %v", err)
	}
	// The reachable positive: the same call as an owner does not refuse for
	// this reason. Without it, a broken engine would pass the assertion above.
	if _, err := e.evaluateProvidersReloadExpression(clusterOwnerCtx("user-owner"), nil); err != nil &&
		strings.Contains(err.Error(), "owner-only") {
		t.Errorf("an owner was refused as a non-owner: %v", err)
	}
}

func TestProviderAuthStatusIsOwnerOnly(t *testing.T) {
	e := engineWithProviders(t, events.NewBus())
	if _, err := e.evaluateProviderAuthStatusExpression(nonClusterOwnerCtx("user-writer")); err == nil {
		t.Fatal("a non-owner read the map of which vendors this deployment talks to")
	}
	if _, err := e.evaluateProviderAuthStatusExpression(clusterOwnerCtx("user-owner")); err != nil {
		t.Errorf("an owner was refused: %v", err)
	}
}

func TestProviderVerifyIsOwnerOnly(t *testing.T) {
	e := engineWithProviders(t, events.NewBus())
	if _, err := e.evaluateProviderVerifyExpression(nonClusterOwnerCtx("user-writer"), map[string]any{"provider": "x"}); err == nil {
		t.Fatal("a non-owner drove an authenticated call to an AI vendor")
	}
}

// TestProviderAuthStatusNamesTheTierNotTheValue is the security-shaped
// assertion: the projection describes a credential without ever carrying one.
func TestProviderAuthStatusNamesTheTierNotTheValue(t *testing.T) {
	const secret = "sk-test-this-must-never-be-rendered"
	resolver := newSeededResolver()
	resolver.seed("MEMQL_AI_OPENAI_API_KEY", secret)
	installSeededResolver(t, resolver)

	e := engineWithProviders(t, events.NewBus())
	registerParsedProviders(nil, e.providers, []parsedProviderConfig{
		{cfg: &ProviderConfig{
			Name: "openai", Type: "OpenAI", Base: true,
			Auth: map[string]string{"apiKey": "${MEMQL_AI_OPENAI_API_KEY}"},
		}, origin: "test:openai"},
		{cfg: &ProviderConfig{
			Name: "chatTest", Extends: "openai", Model: "gpt-test",
			Auth: map[string]string{"apiKey": "${MEMQL_AI_OPENAI_API_KEY}"},
		}, origin: "test:chatTest"},
	})

	nodes, err := e.evaluateProviderAuthStatusExpression(clusterOwnerCtx("user-owner"))
	if err != nil {
		t.Fatalf("providerAuthStatus: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected one row (the base is metadata and must not be listed), got %d", len(nodes))
	}
	payload := string(nodes[0].Payload)
	if strings.Contains(payload, secret) {
		t.Fatal("the projection rendered the credential itself")
	}
	// The reachable positive: it DID resolve, so a test finding no secret is
	// not simply describing an empty row.
	if !strings.Contains(payload, `"available":true`) {
		t.Fatalf("the provider should be available with a seeded key: %s", payload)
	}
	if !strings.Contains(payload, `"authSource":"globalSecret"`) {
		t.Fatalf("the tier should be reported as globalSecret: %s", payload)
	}
}

func TestProviderAuthSourceReportsUnresolvedWhenKeyless(t *testing.T) {
	resolver := newSeededResolver()
	installSeededResolver(t, resolver)
	// Blank the env tier too, or a developer machine with a real key exported
	// turns this green for the wrong reason.
	for _, name := range []string{"MEMQL_AI_OPENAI_API_KEY", "MEMQL_OPENAI_API_KEY", "OPENAI_API_KEY", "MEMQL_SI_OPENAI_API_KEY"} {
		t.Setenv(name, "")
	}

	e := engineWithProviders(t, events.NewBus())
	registerParsedProviders(nil, e.providers, []parsedProviderConfig{
		{cfg: &ProviderConfig{
			Name: "chatTest", Type: "OpenAI", Model: "gpt-test",
			Auth: map[string]string{"apiKey": "${MEMQL_AI_OPENAI_API_KEY}"},
		}, origin: "test:chatTest"},
	})

	nodes, err := e.evaluateProviderAuthStatusExpression(clusterOwnerCtx("user-owner"))
	if err != nil {
		t.Fatalf("providerAuthStatus: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected one row, got %d", len(nodes))
	}
	payload := string(nodes[0].Payload)
	if !strings.Contains(payload, `"authSource":"unresolved"`) {
		t.Errorf("a keyless node should report unresolved: %s", payload)
	}
	if !strings.Contains(payload, `"available":false`) {
		t.Errorf("a keyless node should report unavailable: %s", payload)
	}
	// And it must say WHY -- an unavailable row with an empty reason is the
	// state an operator cannot act on.
	if strings.Contains(payload, `"reason":""`) {
		t.Errorf("an unavailable provider must carry a reason: %s", payload)
	}
}

// TestReloadIsAtomicUnderConcurrentReads exercises the build-then-swap claim.
//
// It cannot prove the absence of a race by observation, so it does the one
// thing that IS conclusive: it runs readers against a reloading registry under
// `-race`, which fails the build if the swap is unsynchronized. The value
// assertion beside it is that a reader never sees a half-populated registry --
// the count is either the old one or the new one, never something between.
func TestReloadIsAtomicUnderConcurrentReads(t *testing.T) {
	reg := newProviderRegistry("")
	registerParsedProviders(nil, reg, []parsedProviderConfig{
		{cfg: &ProviderConfig{Name: "a", Type: "OpenAI", Model: "m", Auth: map[string]string{"apiKey": "k"}}, origin: "t:a"},
	})

	next := newProviderRegistry("")
	registerParsedProviders(nil, next, []parsedProviderConfig{
		{cfg: &ProviderConfig{Name: "a", Type: "OpenAI", Model: "m", Auth: map[string]string{"apiKey": "k"}}, origin: "t:a"},
		{cfg: &ProviderConfig{Name: "b", Type: "OpenAI", Model: "m", Auth: map[string]string{"apiKey": "k"}}, origin: "t:b"},
		{cfg: &ProviderConfig{Name: "c", Type: "OpenAI", Model: "m", Auth: map[string]string{"apiKey": "k"}}, origin: "t:c"},
	})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var bad int32
	var badMu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				n := reg.Count()
				if n != 1 && n != 3 {
					badMu.Lock()
					bad++
					badMu.Unlock()
				}
				// Names() walks the map; under an unsynchronized swap this is
				// where the race detector fires.
				_ = reg.Names()
			}
		}()
	}
	time.Sleep(10 * time.Millisecond)
	reg.adoptContents(next)
	time.Sleep(10 * time.Millisecond)
	close(stop)
	wg.Wait()

	if bad != 0 {
		t.Errorf("a reader saw %d intermediate registry states; the swap is not atomic", bad)
	}
	if reg.Count() != 3 {
		t.Errorf("after the swap the registry should hold the new contents, got %d", reg.Count())
	}
	// The engine's pointer is unchanged -- which is what makes the ~57
	// unsynchronized reads of MemQLEngine.providers still correct.
	if _, ok := reg.Entry("b"); !ok {
		t.Error("the swapped-in contents are not visible through the ORIGINAL registry pointer")
	}
}

// TestProviderBuiltinsAreNotAICallable pins the boundary that keeps an AGENT
// away from these five (epic memql#4440).
//
// WHY IT IS WORTH A TEST OF ITS OWN. registerFunctionTools already skips every
// builtin -- "the MCP connector surface is a curated @mcp opt-in on TOOLS", and
// its comment says so. That exclusion is general and it is not new. What IS
// new is the consequence of it lapsing: `providerKeySet` seals a vendor
// credential into the graph and `providersReload` rotates what an entire fleet
// authenticates with. Before this epic the worst an auto-registered builtin
// could do was read something; these are the first that write a credential.
//
// The owner gate would still refuse an agent acting as an ordinary user, so
// this is defence in depth rather than the only wall. It is here because the
// two walls fail independently: the gate is a runtime check on one caller, and
// this is a structural claim about what is reachable at all.
func TestProviderBuiltinsAreNotAICallable(t *testing.T) {
	functions := newFunctionRegistry()
	tools := newToolRegistry()

	names := []string{
		"providerAuthStatus", "providersReload", "providerVerify",
		"providerKeySet", "providerFederationSet",
	}
	for _, name := range names {
		if err := functions.Upsert(&Function{
			Name:    name,
			Type:    FunctionTypeBuiltin,
			Enabled: true,
		}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	registerFunctionTools(nil, functions, tools)

	for _, name := range names {
		if tools.Has(name) {
			t.Errorf("%q was auto-registered as an AI-callable tool. These builtins seal and "+
				"rotate vendor credentials; an agent must not be able to call them at all, "+
				"quite apart from the owner gate that would refuse it.", name)
		}
	}

	// THE REACHABLE POSITIVE. registerFunctionTools must still do its job for
	// the constructs it is for -- otherwise this test passes against a
	// function that generates nothing, and would keep passing if the builtin
	// exclusion were removed and the whole generator broke instead.
	if err := functions.Upsert(&Function{
		Name: "someOrdinaryQuery",
		// FunctionTypeUserDefined ("") is what a .memql query carries; the
		// generator keys off NOT being a builtin, so this is the control.
		Type: FunctionTypeUserDefined,
		// A description is required: ValidateTool refuses a tool without one,
		// and the generator skips a tool that fails validation. Without this
		// the control would be skipped for its OWN reason and the positive
		// would be unreachable in a second, quieter way.
		Description:  "A control function, so this test cannot pass against a generator that does nothing.",
		FunctionKind: "query",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("register the control query: %v", err)
	}
	registerFunctionTools(nil, functions, tools)
	if !tools.Has("someOrdinaryQuery") {
		t.Fatal("registerFunctionTools generated nothing for an ordinary query; " +
			"the assertions above would pass against a generator that does nothing at all")
	}
}
