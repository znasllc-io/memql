package memql

// memql#2633: the doc-comment side channel is wired through the REAL engine
// load path (function_loader's own lexer/parser pair), not only ParseFile --
// without this wiring FunctionDef.DocComment would be empty everywhere the
// engine actually loads from.

import (
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

func TestFunctionLoader_CapturesDocComment(t *testing.T) {
	src := strings.Join([]string{
		"/// Loader-altitude doc.",
		"logic docProbeLogic {",
		"  args {",
		"    a string @required",
		"  }",
		"  return args.a ?? \"\"",
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

// Builtins take the same parser and converter path as the real registry.
// Dropping this documentation makes working capabilities undiscoverable.
func TestBuiltinLoaderPreservesCapabilityDocumentation(t *testing.T) {
	for _, tc := range []struct{ source, want, doc string }{
		{"/// Create an exported record.\n@executor(\"integration.example.export\")\nbuiltin exportRecord {}", "Create an exported record.", "Create an exported record."},
		{"/// Read the saved file.\n@description(\"Legacy description.\")\n@executor(\"integration.example.read\")\nbuiltin readRecord {}", "Read the saved file.", "Read the saved file."},
		{"@description(\"Legacy description.\")\n@executor(\"integration.example.read\")\nbuiltin legacyRead {}", "Legacy description.", ""},
	} {
		decl, err := languageParser.ParseBuiltinDecl(tc.source)
		if err != nil {
			t.Fatal(err)
		}
		fn, err := builtinDeclToFunction(decl, "example/builtins.memql")
		if err != nil {
			t.Fatal(err)
		}
		if fn.Description != tc.want || fn.DocComment != tc.doc {
			t.Fatalf("description=%q doc=%q; want %q / %q", fn.Description, fn.DocComment, tc.want, tc.doc)
		}
	}
}
