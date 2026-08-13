package memql

// integration_executor_audit_test.go -- memql#3614 item 6.
//
// `@executor("integration.X.Y")` was exempt from load validation:
// initBuiltinExecutorHandlers skipped every `integration.`-prefixed executor
// outright, so 70 builtins across 26 integrations bypassed the gate that
// refuses an unknown native executor loudly. Both cases from the issue --
// `integration.zzautoNoSuchIntegration.doThing` and the trailing-empty
// `integration.zzautoNoSuchIntegration.` -- loaded with the builtin live.

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/component"
)

func auditTestHandler(_ context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return nil, nil
}

func auditTestEngine(t *testing.T, fns ...*Function) *MemQLEngine {
	t.Helper()
	reg := newFunctionRegistry()
	for _, fn := range fns {
		if err := reg.add(fn); err != nil {
			t.Fatalf("add builtin %q: %v", fn.Name, err)
		}
	}
	return &MemQLEngine{
		Component:    &component.Component{Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))},
		initialized:  true,
		functions:    reg,
		integrations: newIntegrationRegistry(),
	}
}

// TestIntegrationExecutorShape_RefusedAtLoad pins the build-tag-INDEPENDENT
// half. A malformed FQN names a capability no binary can ever register, in any
// build, so it is a typo by construction and refused where the native
// executors are: initBuiltinExecutorHandlers, before any integration has had a
// chance to register.
func TestIntegrationExecutorShape_RefusedAtLoad(t *testing.T) {
	cases := []struct {
		name     string
		executor string
	}{
		{name: "trailing-empty-capability", executor: "integration.zzautoNoSuchIntegration."},
		{name: "no-capability-segment", executor: "integration.zzautoNoSuchIntegration"},
		{name: "empty-integration-name", executor: "integration..doThing"},
		{name: "extra-segment", executor: "integration.foo.bar.baz"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := auditTestEngine(t, &Function{
				Name:     "shapeProbe",
				Type:     FunctionTypeBuiltin,
				Executor: tc.executor,
				Enabled:  true,
			})
			err := engine.initBuiltinExecutorHandlers()
			if err == nil {
				t.Fatalf("expected a refusal for malformed executor %q", tc.executor)
			}
			if !strings.Contains(err.Error(), "malformed integration executor") {
				t.Errorf("expected a malformed-executor refusal, got: %v", err)
			}
		})
	}
}

// TestIntegrationExecutorShape_WellFormedStillLoads keeps the shape gate from
// swallowing the normal case: a well-formed integration executor is registered
// dynamically AFTER engine startup, so it must pass here with no registry
// consulted at all.
func TestIntegrationExecutorShape_WellFormedStillLoads(t *testing.T) {
	engine := auditTestEngine(t, &Function{
		Name:     "wellFormedProbe",
		Type:     FunctionTypeBuiltin,
		Executor: "integration.knowledge.searchChunks",
		Enabled:  true,
	})
	if err := engine.initBuiltinExecutorHandlers(); err != nil {
		t.Fatalf("a well-formed integration executor must load: %v", err)
	}
}

// TestAuditIntegrationExecutors_PresentIntegrationMissingCapabilityIsAnError
// pins the half that IS decidable per-binary. The integration is registered in
// this process and does not offer the named capability -- and no provider's
// Capabilities() list is build-tag-gated anywhere in the tree, so that is a
// typo in every build. The binary that can see it is the one that should say
// so.
func TestAuditIntegrationExecutors_PresentIntegrationMissingCapabilityIsAnError(t *testing.T) {
	engine := auditTestEngine(t, &Function{
		Name:     "typoProbe",
		Type:     FunctionTypeBuiltin,
		Executor: "integration.knowledge.searchChunkz",
		Enabled:  true,
	})
	if err := engine.integrations.Register(&mockProvider{
		name: "knowledge",
		capabilities: []IntegrationCapability{
			{Name: "searchChunks", Handler: auditTestHandler},
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	err := engine.AuditIntegrationExecutors()
	if err == nil {
		t.Fatal("expected an error: integration knowledge IS registered and has no searchChunkz capability")
	}
	for _, want := range []string{"typoProbe", "integration.knowledge.searchChunkz", "integration.knowledge.searchChunks"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %q so the fix is obvious, got: %v", want, err)
		}
	}
}

// TestAuditIntegrationExecutors_AbsentIntegrationIsNotFatal is the constraint
// that shapes the whole design. Every binary loads the SAME DSL tree while
// registering only the integrations its build tags compile in, so a planner
// node legitimately holds no integration.cognition.* handler. Failing there is
// not a caught typo, it is an outage on every node type.
func TestAuditIntegrationExecutors_AbsentIntegrationIsNotFatal(t *testing.T) {
	engine := auditTestEngine(t,
		&Function{
			Name:     "cognitionProbe",
			Type:     FunctionTypeBuiltin,
			Executor: "integration.cognition.scoreUtterance",
			Enabled:  true,
		},
		&Function{
			Name:     "typoProbe",
			Type:     FunctionTypeBuiltin,
			Executor: "integration.zzautoNoSuchIntegration.doThing",
			Enabled:  true,
		},
	)

	if err := engine.AuditIntegrationExecutors(); err != nil {
		t.Fatalf("an unregistered integration must WARN, not fail -- build tags exclude integrations by design: %v", err)
	}
}

// TestAuditIntegrationExecutors_RegisteredCapabilityIsClean is the happy path:
// once the integration is wired, nothing is reported at all.
func TestAuditIntegrationExecutors_RegisteredCapabilityIsClean(t *testing.T) {
	engine := auditTestEngine(t, &Function{
		Name:     "goodProbe",
		Type:     FunctionTypeBuiltin,
		Executor: "integration.knowledge.searchChunks",
		Enabled:  true,
	})
	if err := engine.integrations.Register(&mockProvider{
		name: "knowledge",
		capabilities: []IntegrationCapability{
			{Name: "searchChunks", Handler: auditTestHandler},
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := engine.AuditIntegrationExecutors(); err != nil {
		t.Fatalf("a registered capability must audit clean: %v", err)
	}
}

// TestAuditIntegrationExecutors_DisabledBuiltinIsSkipped mirrors the #2608
// carve-out on the native path: retiring a builtin whose backing executor is
// gone is the single most likely reason to disable one, and must not brick
// boot.
func TestAuditIntegrationExecutors_DisabledBuiltinIsSkipped(t *testing.T) {
	engine := auditTestEngine(t, &Function{
		Name:     "retiredProbe",
		Type:     FunctionTypeBuiltin,
		Executor: "integration.knowledge.longGoneCapability",
		Enabled:  false,
	})
	if err := engine.integrations.Register(&mockProvider{
		name: "knowledge",
		capabilities: []IntegrationCapability{
			{Name: "searchChunks", Handler: auditTestHandler},
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := engine.AuditIntegrationExecutors(); err != nil {
		t.Fatalf("a @disabled builtin must skip the audit: %v", err)
	}
}

// TestShippedTree_IntegrationExecutorsAreWellFormed is the corpus guard: every
// `@executor("integration.*")` in the tree parses as
// integration.<integration>.<capability>. Cheap, build-tag-independent, and it
// catches the trailing-dot / missing-segment typo class at the class rather
// than at the call site.
func TestShippedTree_IntegrationExecutorsAreWellFormed(t *testing.T) {
	reg := newFunctionRegistry()
	if _, err := LoadUnifiedBuiltins(nil, reg); err != nil {
		t.Fatalf("LoadUnifiedBuiltins: %v", err)
	}

	checked := 0
	for _, fn := range reg.Snapshot() {
		if fn == nil || !fn.IsBuiltin() || !fn.Enabled {
			continue
		}
		if !strings.HasPrefix(fn.Executor, integrationExecutorPrefix) {
			continue
		}
		checked++
		if err := validateIntegrationExecutorShape(fn.Executor); err != nil {
			t.Errorf("builtin %q: %v", fn.Name, err)
		}
	}
	if checked == 0 {
		t.Fatal("no integration executors found in the shipped tree -- the guard would be vacuous")
	}
}
