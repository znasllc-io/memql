package memql_test

// dsl_construct_source_test.go -- regression coverage for memql#1663: the MCP
// run_automation dry-run path could not resolve an automation's source because
// DSLConstructSource scanned ExtractFunctionSlices (which deliberately skips
// automations) with the kind hardcoded to "automation". The lookup now keys on
// the actual construct kind and routes automation lookups through the dedicated
// ExtractAutomationSlices extractor.

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestDSLConstructSource_AutomationKind: an automation's source is now
// resolvable by (kind="automation", name) -- the dry-run gap. Before #1663 this
// always returned false.
func TestDSLConstructSource_AutomationKind(t *testing.T) {
	logger := quietLogger()

	for _, name := range []string{"registerNode", "expireDelegations", "pruneStaleClusterNodes"} {
		src, ok := memql.DSLConstructSource(logger, "automation", name)
		if !ok {
			t.Fatalf("DSLConstructSource(automation, %q): not found (the #1663 dry-run gap)", name)
		}
		if !strings.Contains(src, "automation "+name) {
			t.Fatalf("DSLConstructSource(automation, %q): source missing its declaration:\n%s", name, src)
		}
		// The sliced automation must carry its step body so the dry-run sandbox
		// can re-parse + execute it.
		if !strings.Contains(src, "logic ") {
			t.Errorf("DSLConstructSource(automation, %q): source missing the logic step body:\n%s", name, src)
		}
	}
}

// TestDSLConstructSource_NonAutomationLogicKind: a non-automation logic
// construct's source is resolvable by (kind="logic", name) -- proving the
// lookup honors the actual kind, not a hardcoded "automation".
func TestDSLConstructSource_NonAutomationLogicKind(t *testing.T) {
	logger := quietLogger()

	src, ok := memql.DSLConstructSource(logger, "logic", "logicRegisterNode")
	if !ok {
		t.Fatalf("DSLConstructSource(logic, logicRegisterNode): not found")
	}
	if !strings.Contains(src, "logic logicRegisterNode") {
		t.Fatalf("DSLConstructSource(logic, logicRegisterNode): unexpected source:\n%s", src)
	}

	// An internal-only logic construct (no wrapping automation) is still
	// source-resolvable by kind.
	if _, ok := memql.DSLConstructSource(logger, "logic", "logicBootstrapCluster"); !ok {
		t.Fatalf("DSLConstructSource(logic, logicBootstrapCluster): not found")
	}

	// A bogus name resolves to nothing.
	if _, ok := memql.DSLConstructSource(logger, "automation", "totallyBogusConstructXyz"); ok {
		t.Fatalf("DSLConstructSource(automation, totallyBogusConstructXyz): unexpectedly found")
	}
}
