//go:build clustere2e

package clustere2e

// Cross-replica RBAC consistency, LIVE (Epic 1, E1.6 / memql#2074).
//
// CONTRACT under test. The consolidated RBAC model (dsl/rbac/, epic memql#2062)
// must resolve IDENTICALLY on every bff replica -- a role's rank and its
// capability grants are global, immutable reference data with no per-node
// state, so an authorization decision can never depend on WHICH replica
// answers. This is the multi-node acceptance for E1.6: governance + capability
// resolution is consistent across the mesh.
//
// nginx round-robins each new gRPC connection across the bff replicas, so
// resolving the same RBAC catalog read on two distinct connections (connA /
// connB) exercises (with high probability) two different replicas. We assert
// the role catalog and a per-role capability grant come back byte-identical
// from both. A regression that let authz drift per-node (a node-local cache
// seeded differently, an env-dependent capability branch) would show up here as
// a divergence.
//
// The deterministic, always-in-CI proof of the same node-agnosticism lives in
// component/auth TestCapableNodeConsistency / TestGovernPrincipalNodeConsistency
// (the model carries no per-node state) and in component/memql
// TestRBACEnforcementWiringLoadsEndToEnd (the chain loads on the boot path);
// this is the live-cluster confirmation over the real 2-replica mesh.
//
// RUN
//
//	MEMQL_E2E_TOKEN=<user JWT> go test -tags clustere2e -count=1 \
//	  -timeout=300s ./test/clustere2e/... -run TestRBACConsistency_CrossReplica
//
// or `make cluster-e2e`.

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

func TestRBACConsistency_CrossReplica(t *testing.T) {
	tok := token(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Several connections so reads land on different replicas behind nginx.
	conns := openConnections(ctx, t, tok, 4)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	qcA := memqlclient.NewQueryClient(conns[0].Dispatcher())
	qcB := memqlclient.NewQueryClient(conns[1].Dispatcher())

	// 1. The role catalog must be identical across replicas.
	rolesA := fingerprintRoles(ctx, t, qcA)
	rolesB := fingerprintRoles(ctx, t, qcB)
	if rolesA != rolesB {
		t.Fatalf("role catalog diverged across replicas:\n  A=%s\n  B=%s", rolesA, rolesB)
	}
	// Sanity: the four base roles are present (so we're actually comparing a
	// loaded catalog, not two empty results).
	for _, slug := range []string{"owner", "developer", "admin", "user"} {
		if !containsRoleSlug(rolesA, slug) {
			t.Fatalf("base role %q missing from the live catalog -- A=%s", slug, rolesA)
		}
	}

	// 2. A per-role capability grant set must be identical across replicas.
	for _, slug := range []string{"owner", "developer", "admin", "user"} {
		capsA := fingerprintCapsForRole(ctx, t, qcA, slug)
		capsB := fingerprintCapsForRole(ctx, t, qcB, slug)
		if capsA != capsB {
			t.Fatalf("capability grants for role %q diverged across replicas:\n  A=%s\n  B=%s", slug, capsA, capsB)
		}
	}
}

// fingerprintRoles resolves the active role catalog and returns a stable,
// order-independent fingerprint of (slug, rank) pairs.
func fingerprintRoles(ctx context.Context, t *testing.T, qc *memqlclient.QueryClient) string {
	t.Helper()
	res, err := qc.ActiveRoles(ctx, memqlclient.ActiveRolesArgs{})
	if err != nil {
		t.Fatalf("ActiveRoles: %v", err)
	}
	var parts []string
	for _, row := range res.Rows() {
		parts = append(parts, fmt.Sprintf("%v=%v", row["slug"], row["rank"]))
	}
	sort.Strings(parts)
	return fmt.Sprintf("%v", parts)
}

func containsRoleSlug(fingerprint, slug string) bool {
	return len(fingerprint) > 0 && (containsSub(fingerprint, "["+slug+"=") || containsSub(fingerprint, " "+slug+"=") || containsSub(fingerprint, slug+"="))
}

// fingerprintCapsForRole resolves a role's active capability grants and returns
// a stable, order-independent fingerprint of (verb, resourceType, effect).
func fingerprintCapsForRole(ctx context.Context, t *testing.T, qc *memqlclient.QueryClient, slug string) string {
	t.Helper()
	res, err := qc.CapabilitiesForRole(ctx, memqlclient.CapabilitiesForRoleArgs{RoleSlug: slug})
	if err != nil {
		t.Fatalf("CapabilitiesForRole(%q): %v", slug, err)
	}
	var parts []string
	for _, row := range res.Rows() {
		parts = append(parts, fmt.Sprintf("%v:%v:%v", row["verb"], row["resourceType"], row["effect"]))
	}
	sort.Strings(parts)
	return fmt.Sprintf("%v", parts)
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
