package workflowhost

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/core/common"
)

func fixture(t *testing.T, sources ...string) Loader {
	t.Helper()
	definitions := map[string]*automations.Automation{}
	for _, source := range sources {
		a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(source, "scope-test.memql")
		if err != nil {
			t.Fatal(err)
		}
		definitions[a.Name] = a
	}
	return func(name string) (*automations.Automation, error) {
		a := definitions[name]
		if a == nil {
			return nil, fmt.Errorf("missing %s", name)
		}
		return a, nil
	}
}

func TestBorrowedScopePreservesAuthorityAndRunAcrossChild(t *testing.T) {
	load := fixture(t, `@template automation parentScope { args { value string! } return automation childScope(value: args.value) }`,
		`@template automation childScope { args { value string! } return builtin scopeProbe(value: args.value) }`)
	for _, internal := range []bool{false, true} {
		ctx := context.Background()
		if internal {
			ctx = auth.ContextWithInternalOrigin(ctx)
		}
		run := common.RunContext{RunId: "existing-run", GoalId: "existing-goal", StepKey: "existing-step", OwnerUserId: "alice", Mode: common.RunModeLive, Override: &common.StepOverride{}}
		ctx = common.ContextWithRun(ctx, run)
		value, err := Run(ctx, "parentScope", map[string]any{"value": "original"}, Options{Load: load, Operations: map[string]Operation{
			"scopeProbe": func(got context.Context, args map[string]any) (any, error) {
				if auth.OriginFromContext(got).IsInternal() != internal {
					t.Fatal("scope upgraded origin")
				}
				actual, ok := common.RunFromContext(got)
				if !ok || !reflect.DeepEqual(actual, run) {
					t.Fatalf("borrowed journal changed: %#v", actual)
				}
				return args["value"], nil
			},
		}})
		if err != nil || value != "original" {
			t.Fatalf("output=%v, err=%v", value, err)
		}
	}
}

func TestScopeRefusesUnboundChildBeforeAnyEffect(t *testing.T) {
	load := fixture(t, `@template automation parentScope { builtin scopeProbe()
automation childScope() }`,
		`@template automation childScope { builtin unrelatedEffect() }`)
	calls := 0
	_, err := Run(context.Background(), "parentScope", nil, Options{Load: load, Operations: map[string]Operation{"scopeProbe": func(context.Context, map[string]any) (any, error) { calls++; return nil, nil }}})
	if err == nil || calls != 0 {
		t.Fatalf("preflight err=%v calls=%d", err, calls)
	}
}

func TestScopePreservesTypedErrorsAndHonorsContinuation(t *testing.T) {
	sentinel := errors.New("provider refused")
	load := fixture(t, `@template automation failingScope { builtin scopeProbe() }`,
		`@template automation continuingScope { builtin scopeProbe() on error continue
return "continued" }`)
	opts := Options{Load: load, Operations: map[string]Operation{"scopeProbe": func(context.Context, map[string]any) (any, error) { return nil, sentinel }}}
	_, err := Run(context.Background(), "failingScope", nil, opts)
	if !errors.Is(err, sentinel) {
		t.Fatalf("lost typed error: %v", err)
	}
	out, err := Run(context.Background(), "continuingScope", nil, opts)
	if err != nil || out != "continued" {
		t.Fatalf("on error continue: %v %v", out, err)
	}
}

func TestScopeArgumentsAndCancellationFailClosed(t *testing.T) {
	load := fixture(t, `@template automation boundedScope { args { value string! } builtin scopeProbe() }`)
	calls := 0
	opts := Options{Load: load, Operations: map[string]Operation{"scopeProbe": func(context.Context, map[string]any) (any, error) { calls++; return nil, nil }}}
	_, err := Run(context.Background(), "boundedScope", nil, opts)
	if err == nil || calls != 0 {
		t.Fatalf("missing argument err=%v calls=%d", err, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Run(ctx, "boundedScope", map[string]any{"value": "x"}, opts)
	if err == nil || calls != 0 {
		t.Fatalf("canceled err=%v calls=%d", err, calls)
	}
}

func TestParallelCallsNeverShareNativeScope(t *testing.T) {
	load := fixture(t, `@template automation boundedScope { return builtin scopeProbe() }`)
	var wg sync.WaitGroup
	for n := 0; n < 24; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := Run(context.Background(), "boundedScope", nil, Options{Load: load, Operations: map[string]Operation{"scopeProbe": func(context.Context, map[string]any) (any, error) { return n, nil }}})
			if err != nil || out != n {
				t.Errorf("scope %d: %v %v", n, out, err)
			}
		}()
	}
	wg.Wait()
}

func TestPublishedScopedCapabilitiesCannotForgeAuthority(t *testing.T) {
	for _, c := range ScopedCapabilities(map[string]Operation{"scopeProbe": nil}) {
		_, err := c.Handler(context.Background(), nil, 0)
		if err == nil || !strings.Contains(err.Error(), "authorized workflow scope") {
			t.Fatalf("direct call: %v", err)
		}
	}
}

func TestScopePreconditionsStopBeforeEffects(t *testing.T) {
	load := fixture(t, `@template automation guardedScope {
 args { permitted bool! }
 precondition allowed {
  check: args.permitted == true
  description: "Caller input permits this recipe."
 }
 builtin scopeProbe()
 }`)
	calls := 0
	opts := Options{Load: load, Operations: map[string]Operation{"scopeProbe": func(context.Context, map[string]any) (any, error) { calls++; return nil, nil }}}
	if _, err := Run(context.Background(), "guardedScope", map[string]any{"permitted": false}, opts); err == nil || calls != 0 {
		t.Fatalf("precondition bypass: %v, %d calls", err, calls)
	}
	if _, err := Run(context.Background(), "guardedScope", map[string]any{"permitted": true}, opts); err != nil || calls != 1 {
		t.Fatalf("permitted recipe: %v, %d calls", err, calls)
	}
}

func TestScopeRejectsChildJournalBeforeParentEffect(t *testing.T) {
	load := fixture(t, `@template automation parentScope {
 builtin scopeProbe()
 automation childScope()
 }`, `@template automation childScope { return true }`)
	child, _ := load("childScope")
	child.JournalRequired = true
	calls := 0
	_, err := Run(context.Background(), "parentScope", nil, Options{Load: load, Operations: map[string]Operation{"scopeProbe": func(context.Context, map[string]any) (any, error) { calls++; return nil, nil }}})
	if err == nil || calls != 0 {
		t.Fatalf("child journal escaped preflight: %v, %d calls", err, calls)
	}
}

func TestRoutingProposalCatalogFailureStopsModelInvocation(t *testing.T) {
	sentinel := errors.New("catalog unavailable")
	calls := 0
	_, err := Run(context.Background(), "routingPolicyProposalWorkflow", map[string]any{"sentence": "prefer local models"}, Options{Operations: map[string]Operation{
		"routingProposalCatalog":        func(context.Context, map[string]any) (any, error) { return nil, sentinel },
		"routingGeneratePolicyProposal": func(context.Context, map[string]any) (any, error) { calls++; return nil, nil },
	}})
	if !errors.Is(err, sentinel) || calls != 0 {
		t.Fatalf("catalog failure: %v, model calls=%d", err, calls)
	}
}

func TestCancelledParallelScopeDoesNotStartWaitingOperation(t *testing.T) {
	load := fixture(t, `@template automation parallelScope {
 for item in [1, 2] parallel(2) {
  builtin scopeProbe()
 }
 }`)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, err := Run(ctx, "parallelScope", nil, Options{Load: load, Operations: map[string]Operation{"scopeProbe": func(context.Context, map[string]any) (any, error) {
			calls++
			if calls == 1 {
				close(entered)
				<-release
			}
			return nil, nil
		}}})
		done <- err
	}()
	<-entered
	cancel()
	close(release)
	<-done
	if calls != 1 {
		t.Fatalf("cancelled waiting operation ran: %d", calls)
	}
}
