package memql

import "testing"

func TestInitBuiltinExecutorHandlers_RejectsUnknownExecutor(t *testing.T) {
	reg := newFunctionRegistry()
	if err := reg.add(&Function{
		Name:     "badBuiltin",
		Type:     FunctionTypeBuiltin,
		Executor: "doesNotExist",
	}); err != nil {
		t.Fatalf("add builtin: %v", err)
	}

	engine := &MemQLEngine{
		initialized: true,
		functions:   reg,
	}
	if err := engine.initBuiltinExecutorHandlers(); err == nil {
		t.Fatalf("expected unknown executor error")
	}
}

func TestInitBuiltinExecutorHandlers_AcceptsLoadedBuiltins(t *testing.T) {
	reg, err := loadEmbeddedFunctions(nil, nil)
	if err != nil {
		t.Fatalf("loadEmbeddedFunctions: %v", err)
	}
	engine := &MemQLEngine{
		initialized: true,
		functions:   reg,
	}
	if err := engine.initBuiltinExecutorHandlers(); err != nil {
		t.Fatalf("initBuiltinExecutorHandlers: %v", err)
	}
	if len(engine.builtinExecutorHandlers) == 0 {
		t.Fatalf("expected builtin handler map to be initialized")
	}
}
