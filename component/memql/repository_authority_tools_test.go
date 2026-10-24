package memql

import "testing"

// Exercise the shipped declarations through the real embedded loader. Native
// integration tests alone cannot detect a namespace omitted from the embed or
// a tool whose advertised fields cannot reach its required builtin contract.
func TestRepositoryAuthorityRequestToolsLoadAndValidate(t *testing.T) {
	tools := newToolRegistry()
	if _, err := LoadUnifiedTools(discardLogger(), tools); err != nil {
		t.Fatal(err)
	}
	functions := newFunctionRegistry()
	if _, err := LoadUnifiedBuiltins(discardLogger(), functions); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"inspectRepositoryAuthority", "requestRepositoryChange", "repositoryChangeRequestStatus", "listRepositoryChangeRequests"} {
		tool, err := tools.Get(name)
		if err != nil {
			t.Fatalf("shipped request tool %s not loaded: %v", name, err)
		}
		targets := toolHandlerTargets(tool)
		if len(targets) != 1 {
			t.Fatalf("%s has no single bounded handler: %v", name, targets)
		}
		fn := resolveFunctionOrAlias(functions, targets[0])
		if fn == nil {
			t.Fatalf("%s target %s is not embedded", name, targets[0])
		}
		if errs := undeclaredBuiltinArgs(tool, fn); len(errs) > 0 {
			t.Fatalf("%s cannot satisfy its native contract: %v", name, errs)
		}
	}
	tool, err := tools.Get("requestRepositoryChange")
	if err != nil {
		t.Fatal(err)
	}
	schema, err := compileToolSchema(tool)
	if err != nil || schema == nil {
		t.Fatalf("request schema: %v", err)
	}
	valid := map[string]any{"sourceId": "connected-source", "requestId": "unique-request", "statement": "Fix the scoped behavior", "operation": "propose_change", "paths": []any{"component/example/"}}
	if err := schema.Validate(valid); err != nil {
		t.Fatalf("bounded request refused: %v", err)
	}
	for _, operation := range []string{"merge", "deploy", "push"} {
		valid["operation"] = operation
		if err := schema.Validate(valid); err == nil {
			t.Errorf("request tool admitted unsupported operation %q", operation)
		}
	}
	valid["operation"] = "propose_change"
	delete(valid, "paths")
	if err := schema.Validate(valid); err == nil {
		t.Fatal("request tool admitted a request without a scope")
	}
}
