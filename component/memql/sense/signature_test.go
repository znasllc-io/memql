package sense

import (
	"strings"
	"testing"
)

// funcCallSource builds a logic body whose cursor sits just after `name(` on
// line 3, so analyzeCursorContext reports ContextFuncCallArgs with ParentFunc
// == name. Returns the source, the 1-based cursor line, and column.
func funcCallSource(name string) (string, int, int) {
	line3 := "    return " + name + "("
	src := "logic wp8 {\n  body {\n" + line3 + "\n  }\n}"
	// Column is 1-based and points just past the '(' (== len(line3)+1).
	return src, 3, len(line3) + 1
}

func TestSignatureHelp_UserFunctionFromArgsSchema(t *testing.T) {
	reg := &stubRegistry{functions: map[string]*FunctionInfo{
		"createSpace": {
			Name:        "createSpace",
			Description: "Create a cognition space.",
			Kind:        "mutation",
			Args: []ArgInfo{
				{Name: "spaceId", Type: "string", Required: true},
				{Name: "name", Type: "string", Required: true},
				{Name: "description", Type: "string", Required: false},
			},
		},
	}}
	s := New(reg)

	src, line, col := funcCallSource("createSpace")
	res := s.SignatureHelp(src, line, col)
	if res == nil {
		t.Fatal("expected a signature result for a user function call")
	}
	if len(res.Signatures) != 1 {
		t.Fatalf("expected 1 signature, got %d", len(res.Signatures))
	}
	sig := res.Signatures[0]
	wantLabel := "createSpace(spaceId string, name string, description string?)"
	if sig.Label != wantLabel {
		t.Errorf("signature label = %q; want %q", sig.Label, wantLabel)
	}
	if len(sig.Parameters) != 3 {
		t.Fatalf("expected 3 parameters (from the args schema), got %d", len(sig.Parameters))
	}
	if sig.Parameters[0].Label != "spaceId string" || sig.Parameters[2].Label != "description string?" {
		t.Errorf("parameter labels wrong: %q ... %q", sig.Parameters[0].Label, sig.Parameters[2].Label)
	}
	if res.ActiveParameter != 0 {
		t.Errorf("active parameter = %d; want 0 (cursor at first arg)", res.ActiveParameter)
	}
}

func TestSignatureHelp_ActiveParameterTracksComma(t *testing.T) {
	reg := &stubRegistry{functions: map[string]*FunctionInfo{
		"f": {Name: "f", Args: []ArgInfo{{Name: "a", Type: "string", Required: true}, {Name: "b", Type: "string", Required: true}}},
	}}
	s := New(reg)
	// Cursor after `f(x, ` -> second argument (index 1).
	line3 := "    return f(x, "
	src := "logic wp8 {\n  body {\n" + line3 + "\n  }\n}"
	res := s.SignatureHelp(src, 3, len(line3)+1)
	if res == nil {
		t.Fatal("expected a signature result")
	}
	if res.ActiveParameter != 1 {
		t.Errorf("active parameter = %d; want 1 (after one comma)", res.ActiveParameter)
	}
}

func TestSignatureHelp_Builtin(t *testing.T) {
	// Pick any builtin that has parameters, so the builtin branch is exercised.
	var name string
	for n, def := range BuiltinFunctions {
		if def.Signature != "" {
			name = n
			break
		}
	}
	if name == "" {
		t.Skip("no builtin with a signature available")
	}
	s := New(&stubRegistry{})
	src, line, col := funcCallSource(name)
	res := s.SignatureHelp(src, line, col)
	if res == nil || len(res.Signatures) != 1 {
		t.Fatalf("expected a builtin signature for %q, got %+v", name, res)
	}
	if res.Signatures[0].Label != BuiltinFunctions[name].Signature {
		t.Errorf("builtin signature label = %q; want %q", res.Signatures[0].Label, BuiltinFunctions[name].Signature)
	}
}

func TestSignatureHelp_NoneOutsideCall(t *testing.T) {
	s := New(&stubRegistry{})
	// Cursor in a plain body, not inside a call.
	if res := s.SignatureHelp("logic x {\n  body {\n    return 1\n  }\n}", 3, 5); res != nil {
		t.Errorf("expected no signature outside a call, got %+v", res)
	}
}

func TestFormatArgList(t *testing.T) {
	got := formatArgList([]ArgInfo{
		{Name: "id", Type: "string", Required: true},
		{Name: "limit", Type: "number", Required: false},
	})
	if want := "id string, limit number?"; got != want {
		t.Errorf("formatArgList = %q; want %q", got, want)
	}
	if got := formatArgList(nil); got != "" {
		t.Errorf("formatArgList(nil) = %q; want empty", got)
	}
	if !strings.Contains(argLabel(ArgInfo{Name: "x", Required: false}), "?") {
		t.Error("optional arg label should carry a trailing ?")
	}
}
