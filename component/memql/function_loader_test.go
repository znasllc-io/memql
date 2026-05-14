package memql

import "testing"

func TestNormalizeBuiltinArgContract_DefaultsToNone(t *testing.T) {
	contract, err := normalizeBuiltinArgContract(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if contract == nil || contract.Profile != BuiltinArgProfileNone {
		t.Fatalf("expected default profile none, got %#v", contract)
	}
}

func TestNormalizeBuiltinArgContract_RequiresStringKeyForStringProfiles(t *testing.T) {
	_, err := normalizeBuiltinArgContract(&builtinArgContractDefinition{
		Profile: string(BuiltinArgProfileStringOrObject),
	})
	if err == nil {
		t.Fatalf("expected error for missing stringKey")
	}
}

func TestLoadBuiltinFunctions_RegistersAliasesAndArgs(t *testing.T) {
	t.Skip("legacy dsl/v1 tree retired; unified-tree coverage lives in component/memql/unified_*_test.go and dsl/embed_test.go.")
	builtins, err := loadBuiltinFunctions(nil)
	if err != nil {
		t.Fatalf("loadBuiltinFunctions: %v", err)
	}
	serviceVersion, ok := builtins["serviceVersion"]
	if !ok || serviceVersion == nil {
		t.Fatalf("expected serviceVersion builtin to be loaded")
	}
	if len(serviceVersion.BuiltinAliases) == 0 || serviceVersion.BuiltinAliases[0] != "memqlVersion" {
		t.Fatalf("expected memqlVersion alias, got %#v", serviceVersion.BuiltinAliases)
	}
	if serviceVersion.BuiltinArgs == nil || serviceVersion.BuiltinArgs.Profile != BuiltinArgProfileNone {
		t.Fatalf("expected serviceVersion args profile none, got %#v", serviceVersion.BuiltinArgs)
	}
}
