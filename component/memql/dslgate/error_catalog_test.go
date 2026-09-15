package dslgate

import (
	"os"
	"strings"
	"testing"
)

func TestErrorCatalogFunctionNeedsNoBuiltinImport(t *testing.T) {
	common, err := os.ReadFile("../../../dsl/common/builtins.memql")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"common/builtins.memql": string(common),
		"demo/logic.memql":      `logic requireValue { return error("value is required") }`,
	}
	if got := gateOn(t, files); len(got) != 0 {
		t.Fatalf("catalog error call needs no import: %v", got)
	}
	// Restoring the conflicting declaration must reproduce the import demand.
	files["common/builtins.memql"] += "\n@executor(\"error\")\nbuiltin error {\n message string!\n}\n"
	got := gateOn(t, files)
	if len(got) != 1 || !strings.Contains(got[0].Detail, "use common.builtins.{ error }") {
		t.Fatalf("restoring the duplicate builtin did not reproduce the import error: %v", got)
	}
}
