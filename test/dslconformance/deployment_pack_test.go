package dslconformance

// deployment_pack_test.go is the I11 (#2225) cohesion guard for the DevOps
// deployment bundle. I11 CENTRALIZED the deploy surface into one `deployment`
// pack: the behavioral half (specs / logic / builtins / actions / automations)
// plus the DATA half that used to live under dsl/cluster/ -- the deploy concepts
// + persistence mutations + read shapes/queries. These tests assert the move is
// complete and id-preserving so a regression that re-splits the surface (or
// re-keys the concept ids) fails here.
//
// The move is ID-PRESERVING: the concept ids stay v1:cluster:deployment /
// v1:cluster:deploymentNodeSpec (the @namespace("cluster") annotation is what
// assembles the id, not the directory), so every downstream consumer -- the
// deployEngineCluster trigger, the createDeployment / updateDeploymentStatus
// mutations, component/deploycontrol, the cockpit deploy command, the SDK --
// is unaffected by the relocation.

import (
	"os"
	"strings"
	"testing"
)

// read returns a DSL file's contents, given a path relative to the `dsl/` tree.
// The relative spelling is what these tests carried from when they ran with the
// DSL tree as their working directory; dslPath is what resolves it now that
// memql#3242 moved the suite up to the root module.
func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(dslPath(t, path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestDeploymentPackIsCohesive asserts the deployment pack owns the FULL deploy
// surface in one place -- every construct kind authored under dsl/deployment/.
func TestDeploymentPackIsCohesive(t *testing.T) {
	for _, f := range []string{
		"deployment/specs.memql",       // auth gates (#1876)
		"deployment/builtins.memql",    // suggestNextVersion (read-only)
		"deployment/logic.memql",       // the deploy decisions (I9)
		"deployment/actions.memql",     // the world-touching primitives (I8)
		"deployment/automations.memql", // deployEngineCluster -- the one orchestrator (I10)
		"deployment/concepts.memql",    // the deploy DATA model (I11 move)
		"deployment/mutations.memql",   // the deploy persistence (I11 move)
		"deployment/shapes.memql",      // the deploy read projections (I11 move)
		"deployment/queries.memql",     // the deploy record reads (I11 move)
	} {
		if _, err := os.Stat(dslPath(t, f)); err != nil {
			t.Errorf("deployment pack is missing %s -- the cohesive pack must own the full deploy surface (I11 #2225): %v", f, err)
		}
	}
}

// TestDeployDataModelLivesInThePack asserts the deploy concepts + mutations are
// AUTHORED in the deployment pack now (not dsl/cluster/), and that the concept
// ids are held stable via @namespace("cluster").
func TestDeployDataModelLivesInThePack(t *testing.T) {
	concepts := read(t, "deployment/concepts.memql")
	for _, want := range []string{
		"concept deployment {",
		"concept deploymentNodeSpec {",
	} {
		if !strings.Contains(concepts, want) {
			t.Errorf("deployment/concepts.memql must declare %q (I11 centralization)", want)
		}
	}
	// ID-preserving: the namespace annotation keeps the canonical id
	// v1:cluster:deployment / v1:cluster:deploymentNodeSpec stable.
	if strings.Count(concepts, `@namespace("cluster")`) < 2 {
		t.Error("deployment concepts must keep @namespace(\"cluster\") so the ids stay v1:cluster:deployment / v1:cluster:deploymentNodeSpec (id-preserving move)")
	}

	mutations := read(t, "deployment/mutations.memql")
	for _, want := range []string{
		"mutate deployment createDeployment {",
		"mutate deployment updateDeploymentStatus {",
		"mutate deploymentNodeSpec createDeploymentNodeSpec {",
		"mutate deploymentNodeSpec updateDeploymentNodeSpec {",
	} {
		if !strings.Contains(mutations, want) {
			t.Errorf("deployment/mutations.memql must declare %q (I11 centralization)", want)
		}
	}
}

// TestClusterNoLongerOwnsTheDeployModel asserts the deploy data model is
// CENTRALIZED, not duplicated: the cluster files must no longer DECLARE the
// deployment concepts / mutations / shapes (a duplicate declaration would be a
// second source of truth and re-split the pack).
func TestClusterNoLongerOwnsTheDeployModel(t *testing.T) {
	for _, c := range []struct {
		file string
		decl string
	}{
		{"cluster/concepts.memql", "concept deployment {"},
		{"cluster/concepts.memql", "concept deploymentNodeSpec {"},
		{"cluster/mutations.memql", "mutate deployment createDeployment {"},
		{"cluster/mutations.memql", "mutate deployment updateDeploymentStatus {"},
		{"cluster/mutations.memql", "mutate deploymentNodeSpec createDeploymentNodeSpec {"},
		{"cluster/queries.memql", "query deployment deploymentById {"},
		{"cluster/queries.memql", "query deployment deploymentsForCluster {"},
	} {
		if strings.Contains(read(t, c.file), c.decl) {
			t.Errorf("%s still declares %q -- the deploy model moved to the deployment pack (I11 #2225); the cluster files must not re-declare it", c.file, c.decl)
		}
	}

	// The cluster topology queries that read a node's payload.deploymentId
	// (query node) deliberately STAY in cluster/ -- they project the
	// v1:cluster:node concept, not the deployment record.
	q := read(t, "cluster/queries.memql")
	for _, stay := range []string{"query node nodesForDeployment {", "query node nodesNotInDeployment {"} {
		if !strings.Contains(q, stay) {
			t.Errorf("cluster/queries.memql must KEEP %q (it reads v1:cluster:node, not the deployment record)", stay)
		}
	}
}
