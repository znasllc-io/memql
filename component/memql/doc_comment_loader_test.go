package memql

// memql#2633: the doc-comment side channel is wired through the REAL engine
// load path (function_loader's own lexer/parser pair), not only ParseFile --
// without this wiring FunctionDef.DocComment would be empty everywhere the
// engine actually loads from.

import (
	"strings"
	"testing"
)

func TestFunctionLoader_CapturesDocComment(t *testing.T) {
	src := strings.Join([]string{
		"/// Loader-altitude doc.",
		"logic docProbeLogic {",
		"  args {",
		"    a string @required",
		"  }",
		"  body {",
		"    return coalesce(args.a, \"\")",
		"  }",
		"}",
	}, "\n")
	fn, err := tryParseNewFunctionSyntax("docProbeLogic", "logic", src, "common.logic.memql", dotAccessLoadRegistry())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if fn == nil {
		t.Fatal("no function")
	}
	if fn.DocComment != "Loader-altitude doc." {
		t.Errorf("DocComment through the loader = %q, want %q", fn.DocComment, "Loader-altitude doc.")
	}
}
