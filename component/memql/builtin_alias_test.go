package memql

import (
	"strings"
	"testing"
)

// #2707: the meta-command shim resolves @alias at lookup, so alias
// collisions must be rejected at load rather than settled silently by
// sorted-name order.

func aliasTestBuiltin(name string, aliases ...string) *Function {
	return &Function{
		Name:           name,
		Type:           FunctionTypeBuiltin,
		Enabled:        true,
		Executor:       "help",
		BuiltinAliases: aliases,
	}
}

func TestValidateBuiltinAliases(t *testing.T) {
	t.Run("duplicate-alias-rejected", func(t *testing.T) {
		reg := newFunctionRegistry()
		if err := reg.Upsert(aliasTestBuiltin("alpha", "shared")); err != nil {
			t.Fatal(err)
		}
		if err := reg.Upsert(aliasTestBuiltin("beta", "Shared")); err != nil {
			t.Fatal(err)
		}
		err := validateBuiltinAliases(reg)
		if err == nil || !strings.Contains(err.Error(), "must be unique") {
			t.Fatalf("duplicate alias must be rejected, got %v", err)
		}
	})
	t.Run("alias-shadowing-primary-rejected", func(t *testing.T) {
		reg := newFunctionRegistry()
		if err := reg.Upsert(aliasTestBuiltin("alpha")); err != nil {
			t.Fatal(err)
		}
		if err := reg.Upsert(aliasTestBuiltin("beta", "alpha")); err != nil {
			t.Fatal(err)
		}
		err := validateBuiltinAliases(reg)
		if err == nil || !strings.Contains(err.Error(), "shadows") {
			t.Fatalf("alias shadowing a primary must be rejected, got %v", err)
		}
	})
	t.Run("clean-registry-passes", func(t *testing.T) {
		reg := newFunctionRegistry()
		if err := reg.Upsert(aliasTestBuiltin("serviceVersion", "memqlVersion")); err != nil {
			t.Fatal(err)
		}
		if err := validateBuiltinAliases(reg); err != nil {
			t.Fatalf("clean alias set must pass: %v", err)
		}
	})
}

// TestLookupBuiltinFunctionAlias pins the shim's alias resolution (#2707):
// exact on the primary name, case-insensitive on @alias, deterministic, and
// clone-free until the single matched entry.
func TestLookupBuiltinFunctionAlias(t *testing.T) {
	reg := newFunctionRegistry()
	if err := reg.Upsert(aliasTestBuiltin("serviceVersion", "memqlVersion")); err != nil {
		t.Fatal(err)
	}
	e := &MemQLEngine{initialized: true, functions: reg}
	for _, spelling := range []string{"memqlVersion", "MEMQLVERSION", "memqlversion"} {
		fn, ok := e.lookupBuiltinFunction(spelling)
		if !ok || fn == nil || fn.Name != "serviceVersion" {
			t.Errorf("lookup(%q) = %v, want the serviceVersion builtin via alias", spelling, fn)
		}
	}
	if _, ok := e.lookupBuiltinFunction("noSuchName"); ok {
		t.Errorf("unknown name must miss")
	}
}
