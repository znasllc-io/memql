package memql

import (
	"strings"
	"testing"
)

// TestParseBuiltinMemQL_GoldenPath locks the canonical struct-form
// builtin syntax: @executor + typed field list.
func TestParseBuiltinMemQL_GoldenPath(t *testing.T) {
	src := []byte(`@enabled
@description("Returns full details for a specific function by name.")
@executor("help")
builtin help {
  name  string  @required
}`)

	fn, err := parseBuiltinMemQL("test.memql", src)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if fn == nil {
		t.Fatal("expected non-nil *Function")
	}
	if fn.Name != "help" {
		t.Errorf("Name = %q, want help", fn.Name)
	}
	if fn.Type != FunctionTypeBuiltin {
		t.Errorf("Type = %q, want FunctionTypeBuiltin", fn.Type)
	}
	if fn.Executor != "help" {
		t.Errorf("Executor = %q, want help", fn.Executor)
	}
}

// TestParseBuiltinMemQL_RejectsLegacyFuncForm locks the deletion of
// the `func (Builtin) name { ... }` form.
func TestParseBuiltinMemQL_RejectsLegacyFuncForm(t *testing.T) {
	src := []byte(`@executor("foo")
func (Builtin) builtinFoo() {
  name string @required
}`)

	_, err := parseBuiltinMemQL("test.memql", src)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "func (Builtin)") {
		t.Errorf("error should mention `func (Builtin)` retirement, got %v", err)
	}
}

// TestParseBuiltinMemQL_RequiresExecutor locks the rule: every
// builtin must declare @executor to identify its Go implementation.
func TestParseBuiltinMemQL_RequiresExecutor(t *testing.T) {
	src := []byte(`@description("no executor")
builtin orphan {
  name string @required
}`)

	_, err := parseBuiltinMemQL("test.memql", src)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "@executor") {
		t.Errorf("error should mention @executor, got %v", err)
	}
}

// TestParseBuiltinMemQL_EmptyBodyMeansProfileNone covers the rule:
// an empty body means profile=none with no args.
func TestParseBuiltinMemQL_EmptyBodyMeansProfileNone(t *testing.T) {
	src := []byte(`@executor("noop")
builtin noop {
}`)

	fn, err := parseBuiltinMemQL("test.memql", src)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if fn == nil {
		t.Fatal("expected non-nil *Function")
	}
	if fn.BuiltinArgs == nil {
		t.Fatal("BuiltinArgs is nil, want non-nil contract")
	}
	if fn.BuiltinArgs.Profile != BuiltinArgProfileNone {
		t.Errorf("Profile = %q, want BuiltinArgProfileNone", fn.BuiltinArgs.Profile)
	}
}

// TestParseBuiltinMemQL_StringOrObjectRequiresStringKey locks
// the validation rule: certain profiles require a stringKey.
func TestParseBuiltinMemQL_StringOrObjectRequiresStringKey(t *testing.T) {
	src := []byte(`@executor("help")
@args(profile="stringOrObject")
builtin help {
  name  string  @required
}`)

	_, err := parseBuiltinMemQL("test.memql", src)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "stringKey") {
		t.Errorf("error should mention stringKey requirement, got %v", err)
	}
}
