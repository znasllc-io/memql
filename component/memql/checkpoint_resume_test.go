package memql

import (
	"context"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

type checkpointProvider struct {
	mockProvider
	capability string
	nilContext bool
}

func (p *checkpointProvider) PrepareCheckpointResume(ctx context.Context, capability string, _ CheckpointResumeRequest) (context.Context, bool, error) {
	p.capability = capability
	if p.nilContext {
		return nil, true, nil
	}
	return ctx, capability == "continue", nil
}

func TestCheckpointResumeRequiresTheRegisteredCapabilityContract(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry(), integrations: newIntegrationRegistry()}
	handler := func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) { return nil, nil }
	p := &checkpointProvider{mockProvider: mockProvider{name: "checkpoint", capabilities: []IntegrationCapability{{Name: "continue", Handler: handler}, {Name: "start", Handler: handler}}}}
	if err := e.integrations.Register(p); err != nil {
		t.Fatal(err)
	}
	for _, fn := range []*Function{
		{Name: "resume", Type: FunctionTypeBuiltin, Executor: "integration.checkpoint.continue"},
		{Name: "repeat", Type: FunctionTypeBuiltin, Executor: "integration.checkpoint.start"},
		{Name: "missing", Type: FunctionTypeBuiltin, Executor: "integration.unregistered.continue"},
		{Name: "wrapper", FunctionKind: "query", Expr: &FunctionCallExpression{Name: "resume"}},
		{Name: "ordinary", Type: FunctionTypeBuiltin, Executor: "concepts"},
	} {
		if err := e.functions.Upsert(fn); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"resume", "repeat", "missing", "wrapper", "ordinary", "absent"} {
		_, accepted, err := e.PrepareCheckpointResume(context.Background(), name, CheckpointResumeRequest{})
		if err != nil || accepted != (name == "resume") {
			t.Fatalf("%s: %v %v", name, accepted, err)
		}
	}
	p.nilContext = true
	if _, accepted, err := e.PrepareCheckpointResume(context.Background(), "resume", CheckpointResumeRequest{}); err == nil || accepted {
		t.Fatal("an invalid preparer result authorized execution")
	}
}
