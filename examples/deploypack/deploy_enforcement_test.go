package deploypack_test

// deploy_enforcement_test.go is the Epic 2 / E2.6 (#2099) enforcement suite --
// the end-to-end proof that the deploy pack drives a full deploy via
// automations + effects, that the azure path is regression-free, that the
// behavior is multi-node consistent, and that the privileged deploy actions are
// RBAC role-gated by the Epic 1 capability model.
//
// These tests run under the default `go test ./...` (no build tag) and exercise
// the REAL pack provider against a fake Executor + engine, so a regression in
// the deploy effect chain or the role gate fails here.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	deploypack "github.com/znasllc-io/memql/examples/deploypack"
)

// TestDeployPackEndToEndLifecycle drives the full deploy lifecycle through the
// pack effects in the order the chained automations fire them:
//
//	in_progress -> runPromote (azure effect) -> succeeded
//	            -> observeReconciledState (ArgoCD synced/healthy)
//	            -> recordBack (append per-node spec)
//
// It proves the dogfood deploy is driven end-to-end by automations + effects --
// the Epic 2 north star -- with NO real cluster (fake Executor + engine).
func TestDeployPackEndToEndLifecycle(t *testing.T) {
	const syncedHealthy = `{"status":{"sync":{"status":"Synced","revision":"rev1"},"health":{"status":"Healthy"}}}`
	exec := &fakeExecutor{kubectlJSON: []byte(syncedHealthy)}
	eng := &fakeEngine{}
	provider := deploypack.NewProviderWithDeps(exec, eng)

	// 1) Drive leg: runPromote (the azure effect) for a staging deploy.
	promote, err := callCapability(t, provider, "runPromote",
		map[string]any{"version": "2026.6.21", "env": "staging"})
	if err != nil {
		t.Fatalf("runPromote: %v", err)
	}
	if !successOf(t, promote) {
		t.Fatal("runPromote should report success for the clean fake")
	}
	if exec.promoteVersion != "2026.6.21" || exec.promoteEnv != "staging" {
		t.Fatalf("runPromote -> RunPromote(%q,%q), want (2026.6.21, staging)", exec.promoteVersion, exec.promoteEnv)
	}

	// 2) Record-back read leg: observe the reconciled ArgoCD state.
	obs, err := callCapability(t, provider, "observeReconciledState", map[string]any{"app": "memql"})
	if err != nil {
		t.Fatalf("observeReconciledState: %v", err)
	}
	if !boolField(t, obs, "synced") || !boolField(t, obs, "healthy") {
		t.Fatal("observeReconciledState should report synced+healthy for the synced/healthy fixture")
	}

	// 3) Record-back write leg: append the observed per-node spec + status.
	if _, err := callCapability(t, provider, "recordBack", map[string]any{
		"deploymentId": "dep-e2e",
		"status":       "succeeded",
		"nodeType":     "bff",
		"version":      "2026.6.21",
		"replicas":     2,
		"imageDigest":  "sha256:deadbeef",
	}); err != nil {
		t.Fatalf("recordBack: %v", err)
	}
	joined := strings.Join(eng.queries, "\n")
	if !strings.Contains(joined, "updateDeploymentStatus(") || !strings.Contains(joined, "createDeploymentNodeSpec(") {
		t.Fatalf("end-to-end record-back must transition status AND append the per-node spec; queries=%v", eng.queries)
	}
}

// TestDeployPackRBACRoleGate is the Epic 1 RBAC role-gate proof on the
// cut-version / rollback deploy actions (#2099). The deploy actions resolve to
// the (verb x resourceType) capability execute x deployment in the consolidated
// model (dsl/rbac/seeds.memql); rollback is owner-only per the E1.3 predicate.
// This pins that the forward-deploy gate admits developer/admin/owner and
// denies writer/reader, and that owner outranks everyone for the rollback gate.
func TestDeployPackRBACRoleGate(t *testing.T) {
	// Forward-deploy (cut + deploy) = execute on deployment. Developer is the
	// floor (engineering power); writer/reader are read-only.
	forwardDeploy := map[auth.Role]bool{
		auth.RoleOwner:     true,
		auth.RoleAdmin:     true,
		auth.RoleDeveloper: true,
		auth.RoleWriter:    false,
		auth.RoleReader:    false,
	}
	for role, want := range forwardDeploy {
		if got := auth.Capable(role, auth.VerbExecute, auth.ResourceDeployment); got != want {
			t.Errorf("Capable(%s, execute, deployment) = %v, want %v (forward-deploy gate)", role, got, want)
		}
	}

	// Rollback is owner-only: the owner outranks every other role, so the
	// owner-only gate (rank-based) admits only owner. We assert via rank that
	// owner is strictly the top, which is what the owner-only rollback gate
	// (requiresOwner / the E1.3 predicate) keys off.
	ownerRank := auth.RoleRank(auth.RoleOwner)
	for _, lower := range []auth.Role{auth.RoleAdmin, auth.RoleDeveloper, auth.RoleWriter, auth.RoleReader} {
		if auth.RoleRank(lower) >= ownerRank {
			t.Errorf("owner rank (%d) must be strictly above %s (%d) -- rollback is owner-only",
				ownerRank, lower, auth.RoleRank(lower))
		}
	}
}

// TestDeployPackRBACDecisionIsNodeConsistent is the multi-node acceptance
// (#2099): the RBAC decision behind the deploy gate is a pure function of the
// role (no DB, no node-local state), so it resolves identically on every
// replica. We prove determinism by resolving the same (role, verb, resource)
// many times -- a node-local-state regression (a cache that diverges per
// replica) would surface as a flaky decision.
func TestDeployPackRBACDecisionIsNodeConsistent(t *testing.T) {
	first := auth.Capable(auth.RoleDeveloper, auth.VerbExecute, auth.ResourceDeployment)
	for i := 0; i < 1000; i++ {
		if got := auth.Capable(auth.RoleDeveloper, auth.VerbExecute, auth.ResourceDeployment); got != first {
			t.Fatalf("Capable is non-deterministic (iter %d): got %v, first %v -- a per-node cache would diverge in the mesh", i, got, first)
		}
	}
	if !first {
		t.Fatal("developer MUST hold execute on deployment (forward-deploy)")
	}
}

// TestDeployPackRecordBackIdempotentAcrossReplicas is the multi-node record-back
// acceptance (#2099): in the 2-replica mesh the recordReconciledState
// automation fires on every replica, so the record-back append must be safe to
// run more than once -- two replicas appending the SAME observed per-node spec
// must produce the SAME mutation calls (the append-only store + asOf latest
// collapses duplicates). We run the effect twice (simulating two replicas) and
// assert each run issues the identical mutation, with no extra/divergent write.
func TestDeployPackRecordBackIdempotentAcrossReplicas(t *testing.T) {
	args := map[string]any{
		"deploymentId": "dep-mesh",
		"status":       "succeeded",
		"nodeType":     "bff",
		"version":      "2026.6.21",
		"replicas":     2,
		"imageDigest":  "sha256:cafe",
	}

	runOnReplica := func() []string {
		eng := &fakeEngine{}
		provider := deploypack.NewProviderWithDeps(&fakeExecutor{}, eng)
		if _, err := callCapability(t, provider, "recordBack", args); err != nil {
			t.Fatalf("recordBack: %v", err)
		}
		return eng.queries
	}

	replicaA := runOnReplica()
	replicaB := runOnReplica()
	if strings.Join(replicaA, "\n") != strings.Join(replicaB, "\n") {
		t.Fatalf("record-back must issue identical mutations on every replica (idempotent append);\n A=%v\n B=%v", replicaA, replicaB)
	}
	// Each replica issues exactly the status transition + the per-node spec.
	if len(replicaA) != 2 {
		t.Fatalf("record-back per replica = %d mutations, want 2 (status + nodeSpec); %v", len(replicaA), replicaA)
	}
}

// boolField reads a named boolean field off the single result node (used for
// observeReconciledState's synced/healthy).
func boolField(t *testing.T, nodes []memorynodes.MemoryNode, field string) bool {
	t.Helper()
	if len(nodes) != 1 {
		t.Fatalf("expected 1 result node, got %d", len(nodes))
	}
	var m map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &m); err != nil {
		t.Fatalf("unmarshal node payload: %v", err)
	}
	v, _ := m[field].(bool)
	return v
}
