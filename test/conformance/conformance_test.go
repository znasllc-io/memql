package conformance

// conformance_test.go -- the single driver that runs every conformance
// dimension as a subtest, counts pass/fail/skip, and publishes the pass-rate
// metric (memql#1715). Any RAN dimension that fails fails the build; DB-backed
// dimensions SKIP (not fail) when no Postgres is reachable.

import (
	"sort"
	"testing"
)

// allChecks is the registry of conformance dimensions. Adding a dimension here
// is all it takes to include it in the gate + the pass-rate.
func allChecks() []check {
	return []check{
		// DB-free dimensions (run everywhere, including go-checks).
		schemaSourceCheck(), // #1713
		systemIDCheck(),     // #1712
		entrypointCheck(),   // #1707
		literalEvalCheck(),  // #1705
		// DB-backed dimensions (run in the CI conformance job).
		lifecycleCheck(),               // #1715 contract
		filterCheck(),                  // #1708
		partialUpdateCheck(),           // #1709
		envelopeCheck(),                // #1710
		piiScrubCheck(),                // #1711
		eventTriggerCheck(),            // #1706
		eventDryRunBindingCheck(),      // #1727
		forgeLogicArgResolutionCheck(), // #1840
		automationLogicFullBodyCheck(), // #1847
	}
}

// TestMCPConformance is THE gate. It builds the rig once and drives every
// dimension, emitting the pass-rate line CI publishes per build.
func TestMCPConformance(t *testing.T) {
	e := newEnv(t)
	checks := allChecks()

	var pass, fail, skip int
	rows := make([]string, 0, len(checks))

	for _, c := range checks {
		c := c
		skipped := c.NeedsDB && !e.HasDB
		label := c.Issue + "/" + c.Dim
		ok := t.Run(label, func(t *testing.T) {
			if skipped {
				t.Skipf("requires Postgres (set MEMORY_NODES_DATABASE_DSN); validated in the CI conformance job")
			}
			c.Run(t, e)
		})
		switch {
		case skipped:
			skip++
			rows = append(rows, "SKIP "+label)
		case ok:
			pass++
			rows = append(rows, "PASS "+label)
		default:
			fail++
			rows = append(rows, "FAIL "+label)
		}
	}

	emitPassRate(t, pass, fail, skip, rows)

	if !e.HasDB {
		t.Logf("NOTE: %d DB-backed dimensions skipped -- run with a Postgres DSN (or in CI) for full coverage", skip)
	}
}

// TestConformanceCoverageReport enumerates the FULL MCP tool surface and reports
// which dimensions cover it, so coverage gaps are explicit rather than implied.
// It never fails the build (coverage is informational); it documents the surface
// the gate runs against and the lifecycle gap to close in follow-ups.
func TestConformanceCoverageReport(t *testing.T) {
	e := newEnv(t)

	// The full @mcp-promoted function surface (every one is covered by the
	// single-schema-source dimension #1713).
	promoted := e.Eng.MCPPromotedFunctionTools()
	names := make([]string, 0, len(promoted))
	for _, m := range promoted {
		if n, _ := m["name"].(string); n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	// The connector tool surface (tools/list) -- meta-tools + reflected + promoted.
	surface := e.listTools(t)

	t.Logf("=== MCP CONFORMANCE COVERAGE REPORT (memql#1715) ===")
	t.Logf("tools/list surface: %d tools", len(surface))
	t.Logf("@mcp-promoted functions: %d (ALL covered by the #1713 single-schema-source dimension)", len(names))
	t.Logf("contract dimensions covered: #1713 #1712 #1707 #1705 #1710 #1709 #1708 #1711 #1706 + lifecycle CRUD")
	t.Logf("lifecycle CRUD (create->read->update->delete, >=2 inputs) currently exercised on: v1:data:record (representative)")
	t.Logf("COVERAGE GAP (follow-up): per-tool CRUD currently covers the record family + (via partial-update/pii dims) user/node/document/identity;")
	t.Logf("  the remaining ~%d promoted constructs are covered for SCHEMA + envelope, not yet for full CRUD lifecycle.", len(names))
	t.Logf("  Cross-node delivery contracts remain owned by test/clustere2e (engine-level conformance does not exercise the mesh hop).")
}
