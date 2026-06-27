package callgraph

import (
	"strings"
	"testing"
)

func rules(fs []Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Rule)
	}
	return out
}

func has(fs []Finding, rule string) bool {
	for _, f := range fs {
		if f.Rule == rule {
			return true
		}
	}
	return false
}

// I2 acceptance: a logic with a write (mutation call) produces a finding.
func TestLogicCallingMutationIsFlagged(t *testing.T) {
	src := `use cluster.mutations.{ createNode }
use cluster.queries.{ existingCluster }
logic registerBad {
  args { event object @required }
  body {
    existing := existingCluster()
    node := createNode({ id: args.event.payload.id })
    return node
  }
}`
	fs := CheckFile("dsl/cluster/logic.memql", src, nil)
	if !has(fs, "logic-purity") {
		t.Fatalf("expected logic-purity finding (logic calls a mutation); got %v", rules(fs))
	}
	// Calling a query is fine -- no finding for existingCluster.
	for _, f := range fs {
		if strings.Contains(f.Message, "existingCluster") {
			t.Fatalf("calling a query must not be flagged: %s", f.Message)
		}
	}
}

// I2 acceptance: a mutation with two writes produces a finding.
func TestMutationWithTwoWritesIsFlagged(t *testing.T) {
	src := `use cluster.concepts.{ node }
mutation node twoWrites {
  args { id string @required }
  insert { id: args.id }
  update { id: args.id, health: "up" }
}`
	fs := CheckFile("dsl/cluster/mutations.memql", src, nil)
	if !has(fs, "mutation-single-write") {
		t.Fatalf("expected mutation-single-write finding; got %v", rules(fs))
	}
}

// A mutation that calls another mutation is a second aggregate write.
func TestMutationCallingMutationIsFlagged(t *testing.T) {
	src := `use cluster.concepts.{ node }
use cluster.mutations.{ createSpawnEvent }
mutation node createNodeBad {
  args { id string @required }
  insert { id: args.id, ev: createSpawnEvent({ nodeId: args.id }) }
}`
	fs := CheckFile("dsl/cluster/mutations.memql", src, nil)
	if !has(fs, "mutation-single-write") {
		t.Fatalf("expected mutation-single-write finding (calls another mutation); got %v", rules(fs))
	}
}

// I2 acceptance: a triggered logic produces a finding.
func TestTriggeredLogicIsFlagged(t *testing.T) {
	src := `@trigger(event="system.startup")
logic onStartupBad {
  body { return true }
}`
	fs := CheckFile("dsl/cluster/logic.memql", src, nil)
	if !has(fs, "trigger-monopoly") {
		t.Fatalf("expected trigger-monopoly finding; got %v", rules(fs))
	}
}

// I2 acceptance: a side-effecting builtin in a query produces a finding (the
// classifier is what I7 will source from sideEffectClass; here it is injected).
func TestSideEffectingBuiltinInQueryIsFlagged(t *testing.T) {
	src := `use cluster.builtins.{ tagRelease }
query node q {
  args { x string }
  body { return tagRelease({ ref: args.x }) }
}`
	classifier := func(name string) bool { return name == "tagRelease" }
	fs := CheckFile("dsl/cluster/queries.memql", src, classifier)
	if !has(fs, "read-only-builtin") {
		t.Fatalf("expected read-only-builtin finding; got %v", rules(fs))
	}
	// With the default (nil) classifier, no builtin finding -- warning-window
	// behavior until I7 supplies sideEffectClass.
	if fs2 := CheckFile("dsl/cluster/queries.memql", src, nil); has(fs2, "read-only-builtin") {
		t.Fatalf("nil classifier must not flag builtins; got %v", rules(fs2))
	}
}

// A compliant logic (calls only a query + a read-only builtin) is clean.
func TestCompliantLogicIsClean(t *testing.T) {
	src := `use cluster.queries.{ existingCluster }
use common.builtins.{ serviceVersion }
logic decide {
  args { event object @required }
  body {
    existing := existingCluster()
    v := serviceVersion({})
    return coalesce(existing.First(), v)
  }
}`
	if fs := CheckFile("dsl/cluster/logic.memql", src, nil); len(fs) != 0 {
		t.Fatalf("compliant logic must be clean; got %v", rules(fs))
	}
}

// Automations are unrestricted: a triggered automation calling a mutation +
// logic is clean.
func TestAutomationIsUnrestricted(t *testing.T) {
	src := `use cluster.mutations.{ createDeployment }
use cluster.logic.{ decide }
@trigger(event="deploy.requested", concept="v1:cluster:deployment")
automation deploy {
  step decide { logic decide { event: event } }
  step record { mutation createDeployment { deploymentId: event.payload.id } }
}`
	if fs := CheckFile("dsl/cluster/automations.memql", src, nil); len(fs) != 0 {
		t.Fatalf("automations are unrestricted; got %v", rules(fs))
	}
}

// Multiple constructs in one file are split and judged independently.
func TestSplitsMultipleConstructs(t *testing.T) {
	src := `use cluster.mutations.{ createNode }
logic clean {
  body { return 1 }
}
logic dirty {
  body { return createNode({ id: "x" }) }
}`
	fs := CheckFile("dsl/cluster/logic.memql", src, nil)
	if len(fs) != 1 || fs[0].Construct != "dirty" {
		t.Fatalf("expected exactly one finding on 'dirty'; got %v", fs)
	}
}
