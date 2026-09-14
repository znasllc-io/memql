package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/envregistry"
	"github.com/znasllc-io/memql/component/memql/readiness"
)

// bootReadinessTestEngine boots a PRIVATE engine rather than borrowing the
// package-wide shared one. SetReadinessIdentity mutates engine-level state,
// and readMergeTestEngine's doc is the authority on which tests may share:
// these may not. Two cases here deliberately claim different node ids, and on
// a shared engine the second would silently answer for the first.
func bootReadinessTestEngine(t *testing.T) *MemQLEngine {
	t.Helper()
	eng, _, _ := readMergeTestEngine(t)
	return eng
}

// readerContext is a signed-in caller with no special standing. The concept is
// public/requiresIdentity, so this is the weakest actor that may read at all --
// which is the one worth reading as, since the setup surface has to tell a
// viewer the truth as plainly as it tells an owner.
func readerContext() context.Context {
	return auth.ContextWithAccess(context.Background(),
		&auth.AccessContext{UserId: "v1:identity:user:reader", Role: auth.RoleReader})
}

func readinessRowsForTest(t *testing.T, e *MemQLEngine) []readiness.NodeReport {
	t.Helper()
	rows, err := e.readModuleReadinessRows(readerContext())
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestBootWritesOneRowPerModuleAndAnUnchangedSweepWritesNone(t *testing.T) {
	e := bootReadinessTestEngine(t)
	ctx := context.Background()
	// A FRESH node id per run: an unchanged verdict younger than
	// readinessRewriteFloor is not restated, so a standing row from an
	// earlier test (or an earlier run of this one against the same database)
	// would make the first write below count 0 and read as a failure.
	nodeId := fmt.Sprintf("readiness-test-node-%d", time.Now().UnixNano())
	e.SetReadinessIdentity(nodeId, "bff")

	manifest, err := envregistry.LoadManifest("")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Modules) == 0 {
		t.Fatal("negative control failed: the manifest declares no modules, so this test would pass on an engine that wrote nothing")
	}
	written, err := e.WriteModuleReadiness(ctx)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if written != len(manifest.Modules) {
		t.Fatalf("wrote %d rows, manifest declares %d modules", written, len(manifest.Modules))
	}

	first := readinessRowsForTest(t, e)
	mine := 0
	for _, r := range first {
		if r.NodeId == nodeId {
			mine++
			switch r.State {
			case readiness.Configured, readiness.Partial, readiness.Unconfigured, readiness.NotApplicable, readiness.Unknown:
			default:
				t.Errorf("%s: state %q is not one of the five", r.Module, r.State)
			}
		}
	}
	if mine != written {
		t.Fatalf("read back %d of my rows, wrote %d", mine, written)
	}

	// A rewrite is a new VERSION of the same ids, and the read collapses to
	// one row per id. If the id were not deterministic this would double.
	// An unchanged, fresh verdict is NOT restated: the second sweep appends no
	// version at all (a production instance, 2026-09-13 -- 772k versions of this
	// concept for a few dozen live ids, every one of them news to nobody).
	rewritten, err := e.WriteModuleReadiness(ctx)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if rewritten != 0 {
		t.Fatalf("an unchanged fresh verdict was rewritten for %d modules; want 0", rewritten)
	}
	second := readinessRowsForTest(t, e)
	mine = 0
	for _, r := range second {
		if r.NodeId == nodeId {
			mine++
		}
	}
	if mine != written {
		t.Fatalf("after a rewrite the read collapsed to %d rows, want %d", mine, written)
	}
}

// The unit-level version of this (TestReportsCarryNoValues) greps a report the
// evaluator returned. This greps a row that made the whole round trip through
// the mutation, the database and the read, which is where a value would
// actually escape.
func TestReadinessRowsCarryNoValues(t *testing.T) {
	e := bootReadinessTestEngine(t)
	t.Setenv("MEMQL_AZURE_BLOB_CONTAINER", "READINESS-SENTINEL-CONTAINER")
	e.SetReadinessIdentity("readiness-sentinel-node", "bff")
	if _, err := e.WriteModuleReadiness(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := readinessRowsForTest(t, e)
	sawMine := false
	for _, r := range rows {
		if r.NodeId == "readiness-sentinel-node" {
			sawMine = true
		}
		raw, _ := json.Marshal(r)
		if strings.Contains(string(raw), "READINESS-SENTINEL-CONTAINER") {
			t.Fatalf("a row carried a resolved value: %s", raw)
		}
	}
	if !sawMine {
		t.Fatal("negative control failed: none of this node's rows came back, so the grep proved nothing")
	}
}

// A report from a node with no v1:cluster:node row must not count. The honest
// answer is then unreported, never unconfigured: not knowing and not being
// configured are different answers, and only one of them sends a person to a
// setup form.
func TestFoldReportsUnreportedWhenNoClusterNodeIsLive(t *testing.T) {
	e := bootReadinessTestEngine(t)
	e.SetReadinessIdentity("readiness-ghost-node", "bff")
	if _, err := e.WriteModuleReadiness(context.Background()); err != nil {
		t.Fatal(err)
	}
	nodes, err := e.evaluateModuleReadinessExpression(readerContext())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) == 0 {
		t.Fatal("the fold answered no modules at all")
	}
	for _, n := range nodes {
		var v readiness.Verdict
		if err := json.Unmarshal(n.Payload, &v); err != nil {
			t.Fatal(err)
		}
		for _, nv := range v.Nodes {
			if nv.NodeId == "readiness-ghost-node" {
				t.Fatalf("module %s counted a node with no live cluster row: %+v", v.Module, v)
			}
		}
	}
}

// The fold's rows are keyed on the module name, so every module the engine
// knows about appears exactly once. A collision would drop a module silently.
func TestFoldAnswersEachModuleExactlyOnce(t *testing.T) {
	e := bootReadinessTestEngine(t)
	nodes, err := e.evaluateModuleReadinessExpression(readerContext())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, n := range nodes {
		if n.ID == "" {
			t.Fatal("a verdict row carries no id; a builtin's id-keyed reply would drop it")
		}
		seen[n.ID]++
	}
	for id, count := range seen {
		if count > 1 {
			t.Errorf("module %q appears %d times in the fold", id, count)
		}
	}
}

func TestRecomputeRefusesAClientOrigin(t *testing.T) {
	e := bootReadinessTestEngine(t)
	ctx := readerContext()
	if _, err := e.Execute(ctx, "builtin readinessRecompute()"); err == nil {
		t.Fatal("a reader over client origin must not be able to force a recompute")
	}
	if _, err := e.Execute(auth.ContextWithInternalOrigin(ctx), "builtin readinessRecompute()"); err != nil {
		t.Fatalf("internal origin must be admitted: %v", err)
	}
}
