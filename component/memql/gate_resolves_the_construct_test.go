package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// THE EXECUTION GATE JUDGES THE CONSTRUCT THAT RUNS.
//
// A builtin's requirements -- its @requiresCapability, the organization it is
// asked about, @serverOnly -- belong to one construct, and the construct that
// runs is the one the call resolved: the engine's by its qualified name, or the
// only one of the bare name. A second namespace declaring the same bare name
// must not change which requirements are judged, and a name the gate cannot
// resolve to exactly one construct is refused, never admitted.

// gatePeerNamespace is a second namespace that declares the same bare names as
// the engine's gated builtins below.
const gatePeerNamespace = "gatepeer"

// gatedEngineBuiltin is one of the engine's gated builtins as the loader
// registers it: under its own namespace, with its requirements.
type gatedEngineBuiltin struct {
	namespace  string
	name       string
	executor   string
	required   CapabilityRequirement
	serverOnly bool
	args       string
}

var gatedEngineBuiltins = []gatedEngineBuiltin{
	// A plain capability requirement.
	{namespace: "repositoryauthority", name: "repositoryChangeAccess", executor: "integration.repositoryauthority.access",
		required: CapabilityRequirement{Verb: auth.VerbRead, Resource: "app:pipelines"}, args: `sourceId: "s"`},
	// A requirement asked about an organization's target.
	{namespace: "platform", name: "siteArchive", executor: "integration.packages.archiveSite",
		required: CapabilityRequirement{Verb: auth.VerbExecute, Resource: "app:deployables/retire"}, args: `siteId: "s", confirmHostname: "h"`},
	// A server-only builtin.
	{namespace: "platform", name: "gateProbeServerOnly", executor: "integration.platform.serverOnlyProbe",
		serverOnly: true, args: `siteId: "s"`},
}

// gateEngineWithPeer is a bare engine holding each gated engine builtin and,
// when withPeer, a second namespace's builtin of the same bare name with no
// requirement at all. reached counts each executor's runs.
func gateEngineWithPeer(t *testing.T, withPeer bool) (*MemQLEngine, map[string]int) {
	t.Helper()
	reached := map[string]int{}
	e := &MemQLEngine{functions: newFunctionRegistry(), initialized: true}
	e.builtinExecutorHandlers = map[string]builtinExecutorHandler{}
	register := func(fn *Function) {
		if err := e.functions.Upsert(fn); err != nil {
			t.Fatalf("register %s: %v", fn.Origin, err)
		}
		executor := fn.Executor
		e.builtinExecutorHandlers[executor] = func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
			reached[executor]++
			return nil, nil
		}
	}
	for _, b := range gatedEngineBuiltins {
		register(&Function{
			Name:               b.name,
			Origin:             "unified:" + b.namespace + "/builtins.memql:" + b.name,
			Type:               FunctionTypeBuiltin,
			FunctionKind:       FunctionTypeBuiltin,
			Executor:           b.executor,
			Enabled:            true,
			RequiresCapability: b.required,
			ServerOnly:         b.serverOnly,
			BuiltinArgs:        &BuiltinArgContract{Profile: BuiltinArgProfileObject},
		})
		if withPeer {
			register(&Function{
				Name:         b.name,
				Origin:       "unified:" + gatePeerNamespace + "/builtins.memql:" + b.name,
				Type:         FunctionTypeBuiltin,
				FunctionKind: FunctionTypeBuiltin,
				Executor:     "integration." + gatePeerNamespace + "." + b.name,
				Enabled:      true,
				BuiltinArgs:  &BuiltinArgContract{Profile: BuiltinArgProfileObject},
			})
		}
	}
	return e, reached
}

// gateCatalog grants every requirement above to the owner and nothing to a
// writer.
func gateCatalog(t *testing.T) {
	t.Helper()
	owner := map[auth.VerbResource]bool{}
	for _, b := range gatedEngineBuiltins {
		if b.required.declared() {
			owner[auth.VerbResource{Verb: b.required.Verb, Resource: b.required.Resource}] = true
		}
	}
	auth.SetCapabilityCatalog(&capabilityFake{
		ranks:  map[string]int{"owner": 400, "writer": 100},
		grants: map[string]map[auth.VerbResource]bool{"owner": owner, "writer": {}},
	})
	t.Cleanup(func() { auth.SetCapabilityCatalog(nil) })
}

// runGatedCall parses call the way a client's call is parsed and executes the
// builtin it plans to, as the top-level builtin branch of executeWith does.
func runGatedCall(e *MemQLEngine, ctx context.Context, call string) error {
	plan, err := e.Parse(call)
	if err != nil {
		return err
	}
	if err := e.refusePlanBelowRequiredCapability(ctx, plan); err != nil {
		return err
	}
	builtin, ok := plan.Root.(*BuiltinFunctionExpression)
	if !ok {
		return nil
	}
	_, err = e.evaluateBuiltinFunctionExpression(ctx, builtin, 0)
	return err
}

func TestACallerWithNoGrantIsRefusedTheEnginesBuiltinWhenAnotherNamespaceSharesItsName(t *testing.T) {
	gateCatalog(t)
	e, reached := gateEngineWithPeer(t, true)
	for _, b := range gatedEngineBuiltins {
		for _, call := range []string{
			b.namespace + "." + b.name + "(" + b.args + ")",
			b.name + "(" + b.args + ")",
		} {
			err := runGatedCall(e, asCaller("writer"), call)
			if err == nil {
				t.Errorf("%s: a caller with no grant was admitted", call)
			}
			if n := reached[b.executor]; n != 0 {
				t.Errorf("%s: the engine's executor ran %d time(s) for a caller with no grant (err: %v)", call, n, err)
			}
		}
	}
	for executor, n := range reached {
		if n != 0 {
			t.Errorf("executor %s ran %d time(s) for a caller with no grant", executor, n)
		}
	}
}

func TestACallerHoldingTheGrantStillReachesTheEnginesOwnBuiltin(t *testing.T) {
	gateCatalog(t)
	e, reached := gateEngineWithPeer(t, false)
	owner := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "v1:identity:user:caller", Role: auth.RoleOwner})
	// The unqualified call to the one construct of the name, and its qualified
	// spelling, both reach it for a holder of its grant.
	for _, call := range []string{`repositoryChangeAccess(sourceId: "s")`, `repositoryauthority.repositoryChangeAccess(sourceId: "s")`} {
		if err := runGatedCall(e, owner, call); err != nil {
			t.Errorf("%s: a holder of the grant was refused: %v", call, err)
		}
	}
	if got := reached["integration.repositoryauthority.access"]; got != 2 {
		t.Errorf("the holder's calls reached the engine's executor %d time(s), want 2", got)
	}
}

// TestTheBuiltinSeamRefusesWhatItCannotResolveToOneConstruct is the execution
// seam alone, the one every builtin execution crosses whatever parsed it.
func TestTheBuiltinSeamRefusesWhatItCannotResolveToOneConstruct(t *testing.T) {
	gateCatalog(t)
	e, reached := gateEngineWithPeer(t, true)
	owner := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "v1:identity:user:caller", Role: auth.RoleOwner})
	engine := gatedEngineBuiltins[0]
	for _, tc := range []struct {
		why  string
		ctx  context.Context
		expr *BuiltinFunctionExpression
	}{
		{"the engine's construct, judged by its own requirement", asCaller("writer"),
			&BuiltinFunctionExpression{Name: engine.name, Executor: engine.executor, Construct: engine.namespace + "." + engine.name}},
		{"a bare name two namespaces declare", owner,
			&BuiltinFunctionExpression{Name: engine.name, Executor: engine.executor}},
		{"a construct key no construct holds", owner,
			&BuiltinFunctionExpression{Name: engine.name, Executor: engine.executor, Construct: "nowhere." + engine.name}},
		{"a construct that does not bind the executor the call dispatches to", owner,
			&BuiltinFunctionExpression{Name: engine.name, Executor: engine.executor, Construct: gatePeerNamespace + "." + engine.name}},
	} {
		if _, err := e.evaluateBuiltinFunctionExpression(tc.ctx, tc.expr, 0); err == nil {
			t.Errorf("%s: admitted", tc.why)
		}
	}
	if n := reached[engine.executor]; n != 0 {
		t.Errorf("the engine's executor ran %d time(s) through calls the seam must refuse", n)
	}
	if _, err := e.evaluateBuiltinFunctionExpression(owner, &BuiltinFunctionExpression{Name: engine.name, Executor: engine.executor, Construct: engine.namespace + "." + engine.name}, 0); err != nil {
		t.Errorf("the holder's call to the engine's construct by its key was refused: %v", err)
	}
	serverOnly := gatedEngineBuiltins[2]
	if _, err := e.evaluateBuiltinFunctionExpression(owner, &BuiltinFunctionExpression{Name: serverOnly.name, Executor: serverOnly.executor, Construct: serverOnly.namespace + "." + serverOnly.name}, 0); err == nil || !strings.Contains(err.Error(), "server-only") {
		t.Errorf("a client's call to a server-only builtin reached past the seam: %v", err)
	}
	if _, err := e.evaluateBuiltinFunctionExpression(auth.ContextWithInternalOrigin(owner), &BuiltinFunctionExpression{Name: serverOnly.name, Executor: serverOnly.executor, Construct: serverOnly.namespace + "." + serverOnly.name}, 0); err != nil {
		t.Errorf("internal origin was refused a server-only builtin: %v", err)
	}
}
