package automations

// cluster_bootstrap_prune_migration_test.go -- behavioral/compile coverage for
// the #2235 logic-purity migration of bootstrapCluster + pruneStaleClusterNodes
// (the last two cluster-lane violators; deregisterNode + registerNode landed in
// PR #2239).
//
// Both logics were made PURE (the call-graph contract, test/dslconformance/callgraph_contract_test.go,
// now enforces it -- their baseline entries were removed) and their per-write
// side effects moved onto the calling automation:
//
//   - bootstrapCluster: the pure logic decides a single boolean `create` (no
//     cluster row yet AND node.type=="bff"). The three creates (database /
//     identityProvider / cluster) are `if`-gated mutation steps. The idp create
//     additionally requires an identityProvider block in the startup envelope.
//
//   - pruneStaleClusterNodes: the pure logic returns the rows past the stale
//     window (window math pushed into the staleClusterNodes query via olderThan).
//     The per-row terminal write is a `forEach` step.
//
// These tests assert two things a load-only gate can't:
//  1. the migrated authored shape compiles to the intended step IR (decide ->
//     pure logic; the writes are if-gated / forEach mutation steps targeting the
//     right mutations), and
//  2. the gating SEMANTICS: the `decide == true` guard fires the creates only
//     when the logic returned true (bff + no cluster), and the idp
//     `&& args.identityProvider != nil` guard additionally requires the
//     envelope block -- so "bff + no cluster" creates the rows and anything else
//     creates nothing.

import (
	"log/slog"
	"os"
	"testing"
)

// bootstrapClusterAutomation mirrors dsl/cluster/automations.memql. The full
// tree (with this exact text) is validated to load by memqllint; here we
// compile it standalone to inspect the step IR.
const bootstrapClusterAutomation = `@enabled
@trigger(event="system.startup")
@description("Bootstrap cluster, database, and identity provider records on first startup")
automation bootstrapCluster {
  args {
    database any
    identityProvider any
    node any
    provider any
  }
  decide := logic bootstrapCluster(event: event)
  if decide == true {
    databaseRecord := mutation createDatabase(
      host:    args.database.host ?? "localhost",
      dbName:  args.database.dbName ?? "memql",
      sslMode: args.database.sslMode ?? "disable"
    )
  }
  if decide == true && args.identityProvider != nil {
    idpRecord := mutation createIdentityProvider(
      name:           args.identityProvider.name ?? "memql-identity",
      issuerUrl:      args.identityProvider.issuerUrl ?? "",
      clientIdPrefix: args.identityProvider.clientIdPrefix ?? "",
      redirectUrl:    args.identityProvider.redirectUrl ?? ""
    )
  }
  if decide == true {
    cluster := mutation createCluster(
      name:        "development",
      environment: "development",
      region:      "local",
      provider:    args.provider ?? "",
      version:     args.node.version ?? ""
    )
  }
}`

const pruneStaleClusterNodesAutomation = `@enabled
@trigger(schedule="0 */10 * * * *")
@description("Every 10 min: mark departed cluster nodes as health='stopped'.")
automation pruneStaleClusterNodes {
  decide := logic pruneStaleClusterNodes(event: event)
  for node in decide {
    mutation updateNodeHealth(
      id:       node.id,
      health:   "stopped",
      lastSeen: now
    )
  }
}`

func compileAuto(t *testing.T, src, id string) *Automation {
	t.Helper()
	loader := NewLoader(LoaderOptions{Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))})
	auto, err := loader.CompileSource(src, id)
	if err != nil {
		t.Fatalf("authored automation %s must compile: %v", id, err)
	}
	return auto
}

func stepByID(auto *Automation, id string) *Step {
	for _, s := range auto.Steps {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// TestBootstrapCluster_CompilesToDecideThenGatedCreates: the migrated authored
// shape lowers to a pure `decide` logic step + three if-gated mutation steps,
// each targeting the right create mutation and gated on the decide result.
func TestBootstrapCluster_CompilesToDecideThenGatedCreates(t *testing.T) {
	auto := compileAuto(t, bootstrapClusterAutomation, "test:bootstrap")

	decide := stepByID(auto, "decide")
	if decide == nil || decide.Function == nil || decide.Function.Name != "bootstrapCluster" {
		t.Fatalf("decide step must invoke the pure `bootstrapCluster` logic; got %+v", decide)
	}
	if decide.Condition != "" {
		t.Errorf("decide step must be ungated, got condition %q", decide.Condition)
	}

	cases := []struct {
		step, mutation, condition string
	}{
		{"databaseRecord", "createDatabase", "decide == true"},
		{"idpRecord", "createIdentityProvider", "decide == true && args.identityProvider != nil"},
		{"cluster", "createCluster", "decide == true"},
	}
	for _, tc := range cases {
		s := stepByID(auto, tc.step)
		if s == nil || s.Function == nil {
			t.Errorf("step %q missing or not a function call: %+v", tc.step, s)
			continue
		}
		if s.Function.Name != tc.mutation {
			t.Errorf("step %q must call %q, got %q", tc.step, tc.mutation, s.Function.Name)
		}
		if s.Condition != tc.condition {
			t.Errorf("step %q condition = %q, want %q", tc.step, s.Condition, tc.condition)
		}
	}
}

// TestBootstrapCluster_GateSemantics proves the runtime gating: given the pure
// logic's decision (the `decide` result) and the trigger payload bound into the
// args, the create steps fire under "bff + no cluster (+ idp present for the
// idp row)" and are skipped otherwise. This is the behavioral half -- that the
// relocated condition reproduces the original `if existing.empty() &&
// node.type=="bff" (&& identityProvider != nil)` guards. The conditions are
// the ones the compile writes on the create steps, evaluated as a statement
// run evaluates them: `decide` a bound name, the payload the args.
func TestBootstrapCluster_GateSemantics(t *testing.T) {
	auto := compileAuto(t, bootstrapClusterAutomation, "test:bootstrap")
	databaseCond := stepByID(auto, "databaseRecord").Condition
	idpCond := stepByID(auto, "idpRecord").Condition

	mkEval := func(create bool, idpPresent bool) *Evaluator {
		e := NewEvaluator()
		e.enterStatements()
		e.Bind("decide", create)
		args := map[string]any{"node": map[string]any{"type": "bff"}}
		if idpPresent {
			args["identityProvider"] = map[string]any{"name": "memql-identity"}
		}
		e.SetCustom("args", args)
		return e
	}

	eval := func(t *testing.T, e *Evaluator, cond string) bool {
		t.Helper()
		got, err := evalV1Cond(t, e, cond)
		if err != nil {
			t.Fatalf("%s: %v", cond, err)
		}
		return got
	}

	// bff + no cluster + idp present: all three fire.
	e := mkEval(true, true)
	if !eval(t, e, databaseCond) {
		t.Error("database create must fire on create=true")
	}
	if !eval(t, e, idpCond) {
		t.Error("identityProvider create must fire on create=true + idp present")
	}

	// bff + no cluster, NO idp block: database/cluster fire, idp skipped.
	e = mkEval(true, false)
	if !eval(t, e, databaseCond) {
		t.Error("database create must fire on create=true (no idp)")
	}
	if eval(t, e, idpCond) {
		t.Error("identityProvider create must be skipped when no identityProvider block is present")
	}

	// create=false (cluster already exists, or non-bff node): nothing fires.
	e = mkEval(false, true)
	if eval(t, e, databaseCond) {
		t.Error("database create must be skipped when create=false")
	}
	if eval(t, e, idpCond) {
		t.Error("identityProvider create must be skipped when create=false")
	}
}

// TestPruneStaleClusterNodes_CompilesToDecideThenForEachWrite: the migrated
// shape lowers to a pure `decide` logic step + a `forEach` step whose per-item
// body is the terminal updateNodeHealth write (the per-row write that used to
// live inline in the logic).
func TestPruneStaleClusterNodes_CompilesToDecideThenForEachWrite(t *testing.T) {
	auto := compileAuto(t, pruneStaleClusterNodesAutomation, "test:prune")

	decide := stepByID(auto, "decide")
	if decide == nil || decide.Function == nil || decide.Function.Name != "pruneStaleClusterNodes" {
		t.Fatalf("decide step must invoke the pure `pruneStaleClusterNodes` logic; got %+v", decide)
	}

	// The forEach lowers to a top-level loop whose step ID the procedural
	// parser synthesizes (forEach_item_<pos>), so find it by type.
	var prune *Step
	for _, s := range auto.Steps {
		if s.Type == StepTypeForEach {
			prune = s
			break
		}
	}
	if prune == nil || prune.ForEach == nil {
		t.Fatalf("prune step must be a forEach; steps=%+v", auto.Steps)
	}
	if prune.ForEach.Source != "decide" {
		t.Errorf("the loop must iterate the decide result, got source %q", prune.ForEach.Source)
	}
	if prune.ForEach.As != "node" {
		t.Errorf("the loop must bind its author's variable, `node`, got %q", prune.ForEach.As)
	}
	if len(prune.ForEach.Do) != 1 || prune.ForEach.Do[0].Function == nil ||
		prune.ForEach.Do[0].Function.Name != "updateNodeHealth" {
		t.Fatalf("forEach body must be a single updateNodeHealth write; got %+v", prune.ForEach.Do)
	}
}

// TestBootstrapCluster_LogicReturnResolvesCreate is the regression net for
// memql#2579. It evaluates the EXACT terminal return authored in
// dsl/cluster/logic.memql --
//
//	return existing.empty() && args.event.payload.node.type == "bff"
//
// -- over the run, as a logic body's return is evaluated, `existing` bound to
// the rows its query statement read. It once fell through
// to engine.Execute, whose converter refused the `existing.empty()`
// collection-method operand with the ADR 2.2 gate; it must resolve to the
// correct `create` boolean. (The sibling TestBootstrapCluster_GateSemantics
// only exercises the automation's `if decide == true` given a decide result;
// it never evaluates the logic body that PRODUCES that boolean.)
func TestBootstrapCluster_LogicReturnResolvesCreate(t *testing.T) {
	const ret = `existing.empty() && args.event.payload.node.type == "bff"`

	mkEval := func(clusterExists bool, nodeType string) *Evaluator {
		e := NewEvaluator()
		var rows []map[string]any
		if clusterExists {
			rows = append(rows, map[string]any{"id": "v1:cluster:cluster:development", "payload": map[string]any{}})
		}
		e.enterStatements()
		e.Bind("existing", functionStatementValue("query", rowsResult(rows...)))
		e.SetCustom("args", map[string]any{
			"event": map[string]any{
				"payload": map[string]any{
					"node": map[string]any{"type": nodeType},
				},
			},
		})
		return e
	}

	cases := []struct {
		name          string
		clusterExists bool
		nodeType      string
		want          bool
	}{
		{"no cluster + bff -> create", false, "bff", true},
		{"cluster exists + bff -> skip", true, "bff", false},
		{"no cluster + agent -> skip", false, "agent", false},
		{"cluster exists + agent -> skip", true, "agent", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := mkEval(tc.clusterExists, tc.nodeType)
			val, err := evalV1(e, ret)
			if err != nil {
				t.Fatalf("%s: %v", ret, err)
			}
			got, ok := val.(bool)
			if !ok {
				t.Fatalf("expected bool result (locks the create boolean sense), got %T (%#v)", val, val)
			}
			if got != tc.want {
				t.Errorf("create = %v, want %v", got, tc.want)
			}
		})
	}
}
