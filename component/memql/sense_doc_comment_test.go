package memql

// memql#2636: sense reads the /// channel end-to-end -- the adapter's
// FunctionInfo.Description (what hover/completion/signature render) carries
// the ///-derived resolved description inherited from the #2634 flip.

import (
	"strings"
	"testing"
)

func TestSenseAdapter_DocCommentDescription(t *testing.T) {
	src := strings.Join([]string{
		"/// Hover renders this doc-comment channel.",
		"@description(\"Not this fallback.\")",
		"logic senseDocProbe {",
		"  args {",
		"    a string @required",
		"  }",
		"  body {",
		"    return coalesce(args.a, \"\")",
		"  }",
		"}",
	}, "\n")
	fn, err := tryParseNewFunctionSyntax("senseDocProbe", "logic", src, "common.logic.memql", dotAccessLoadRegistry())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	e := &MemQLEngine{functions: newFunctionRegistry()}
	if err := e.functions.Upsert(fn); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	adapter := &SenseAdapter{engine: e}
	fi, ok := adapter.FunctionGet("senseDocProbe")
	if !ok || fi == nil {
		t.Fatal("senseDocProbe not resolvable through the sense adapter")
	}
	if fi.Description != "Hover renders this doc-comment channel." {
		t.Errorf("sense FunctionInfo.Description = %q, want the /// channel", fi.Description)
	}
}
