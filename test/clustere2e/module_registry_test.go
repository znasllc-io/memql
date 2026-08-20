//go:build clustere2e

package clustere2e

// module_registry_test.go -- cluster-parity coverage for the module
// registry (memql#4188 cross-node read, memql#4189 pack flip, memql#4190
// harness-as-pack), against the 2-replica k3d cluster like every gate in
// this package.
//
// WHAT THE FLIP TEST ASSERTS, AND WHAT IT DELIBERATELY DOES NOT: pack
// enablement is restart-required by design (module-registry design
// section 4.2) -- the write lands a v1:platform:packState row every node
// reads at its NEXT boot. So the test asserts the cluster-scope STATE
// (disabled, then re-enabled) agrees across connections landing on
// different replicas, and that loaded/inert stays honest ("state changed
// since this node booted"); it does NOT expect a live unload, because no
// such thing exists. The cluster is left ENABLED on exit -- including on
// assertion failure, via a deferred restore -- so a red run cannot strand
// the shared cluster with a disabled harness.
//
// Env gates match the package: MEMQL_E2E_TOKEN (owner; skip without),
// MEMQL_E2E_ADMIN_TOKEN (a NON-owner caller, for the refusal case; that
// leg skips without it).
//
// RUN
//
//	MEMQL_E2E_TOKEN=<owner JWT> [MEMQL_E2E_ADMIN_TOKEN=<non-owner JWT>] \
//	  go test -tags clustere2e -run TestModule -count=1 -timeout=300s \
//	  ./test/clustere2e/...

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/sdk/go/modules"
)

// modulesClients opens n connections (nginx round-robins them across the
// bff replicas) and wraps each in a modules client.
func modulesClients(ctx context.Context, t *testing.T, tok string, n int) []*modules.Client {
	t.Helper()
	conns := openConnections(ctx, t, tok, n)
	out := make([]*modules.Client, 0, len(conns))
	for _, c := range conns {
		out = append(out, modules.NewClient(c.Dispatcher()))
	}
	return out
}

// clusterScopeFingerprint reduces an inventory to its CLUSTER-scope rows'
// (kind, name, state) -- the facts that must agree regardless of which
// replica answered. Node-scope rows are each binary's own truth and are
// deliberately excluded; reporting node ids are expected to differ.
func clusterScopeFingerprint(inv *modules.Inventory) string {
	var rows []string
	for _, m := range inv.Modules {
		if m.Scope != "cluster" {
			continue
		}
		rows = append(rows, fmt.Sprintf("%s/%s=%s", m.Kind, m.Name, m.State))
	}
	sort.Strings(rows)
	return strings.Join(rows, "\n")
}

// TestModulesInventory_CrossReplica: the inventory read lands on multiple
// replicas; cluster-scope rows byte-agree, and the harness pack row is
// present (memql#4190 made it a pack everywhere).
func TestModulesInventory_CrossReplica(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	clients := modulesClients(ctx, t, tok, 4)

	var fingerprints []string
	reportingNodes := map[string]struct{}{}
	for i, c := range clients {
		inv, err := c.List(ctx)
		if err != nil {
			t.Fatalf("conn %d: modules list: %v", i, err)
		}
		reportingNodes[inv.ReportingNodeID] = struct{}{}
		fingerprints = append(fingerprints, clusterScopeFingerprint(inv))

		foundHarness := false
		for _, m := range inv.Modules {
			if m.Kind == "pack" && m.Name == "harness" {
				foundHarness = true
			}
		}
		if !foundHarness {
			t.Errorf("conn %d: inventory carries no harness pack row", i)
		}
	}
	for i := 1; i < len(fingerprints); i++ {
		if fingerprints[i] != fingerprints[0] {
			t.Errorf("cluster-scope inventory disagrees between conn 0 and conn %d:\n--- conn 0 ---\n%s\n--- conn %d ---\n%s",
				i, fingerprints[0], i, fingerprints[i])
		}
	}
	// With 4 connections across 2 replicas, all landing on one replica is
	// possible (p=2^-3 per run) -- so log rather than assert the spread, and
	// let the byte-agreement above carry the cross-replica claim when the
	// spread happened.
	t.Logf("inventory answered by %d distinct node(s)", len(reportingNodes))
}

// TestSetPackEnabled_CrossReplica: an owner disables the harness through
// one connection; a DIFFERENT connection reads state=disabled with the
// restart-required honesty; re-enable restores it; a non-owner is refused
// and the refusal changes nothing.
func TestSetPackEnabled_CrossReplica(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	clients := modulesClients(ctx, t, tok, 3)
	writer, readerA, readerB := clients[0], clients[1], clients[2]

	// Leave the cluster ENABLED no matter how this test exits.
	defer func() {
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer restoreCancel()
		if _, err := writer.SetPackEnabled(restoreCtx, "harness", true, "clustere2e restore"); err != nil {
			t.Errorf("RESTORE FAILED -- the cluster may be left with a disabled harness: %v", err)
		}
	}()

	flip, err := writer.SetPackEnabled(ctx, "harness", false, "clustere2e: cross-replica flip")
	if err != nil {
		t.Fatalf("owner disable: %v", err)
	}
	if flip.Enabled || !flip.RestartRequired {
		t.Fatalf("disable flip shape wrong: %+v", flip)
	}

	assertHarnessState := func(c *modules.Client, wantState string, label string) {
		t.Helper()
		inv, err := c.List(ctx)
		if err != nil {
			t.Fatalf("%s: list: %v", label, err)
		}
		for _, m := range inv.Modules {
			if m.Kind == "pack" && m.Name == "harness" {
				if m.State != wantState {
					t.Errorf("%s (node %s): harness state = %q, want %q (detail: %s)",
						label, inv.ReportingNodeID, m.State, wantState, m.StateDetail)
				}
				if wantState == "disabled" && !strings.Contains(m.StateDetail, "restart") {
					t.Errorf("%s: disabled-but-still-loaded must say restart-required, got %q", label, m.StateDetail)
				}
				return
			}
		}
		t.Errorf("%s: no harness pack row", label)
	}

	// The graph is the shared truth: every connection -- whichever replica
	// answers -- reads the same desired state.
	assertHarnessState(readerA, "disabled", "reader A after disable")
	assertHarnessState(readerB, "disabled", "reader B after disable")

	// Non-owner refusal (when a second credential is provided): blocked,
	// audited server-side, and the state does not move.
	if adminTok := os.Getenv("MEMQL_E2E_ADMIN_TOKEN"); adminTok != "" {
		nonOwner := modulesClients(ctx, t, adminTok, 1)[0]
		_, err := nonOwner.SetPackEnabled(ctx, "harness", true, "must be refused")
		var refused *modules.RefusedError
		if !errors.As(err, &refused) {
			t.Errorf("non-owner flip must be refused, got %v", err)
		}
		assertHarnessState(readerA, "disabled", "reader A after refused flip")
	} else {
		t.Log("MEMQL_E2E_ADMIN_TOKEN not set; skipping the non-owner refusal leg")
	}

	// Re-enable and confirm from the other connection.
	reFlip, err := writer.SetPackEnabled(ctx, "harness", true, "clustere2e: re-enable")
	if err != nil {
		t.Fatalf("owner re-enable: %v", err)
	}
	if !reFlip.Enabled || reFlip.PriorEnabled {
		t.Fatalf("re-enable flip shape wrong: %+v", reFlip)
	}
	assertHarnessState(readerB, "enabled", "reader B after re-enable")
}
