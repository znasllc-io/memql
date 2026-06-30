package callgraph

import "testing"

// A conformant action -- one external capability call, no construct calls, no
// graph write, no trigger -- is clean (construct-invocation ADR Decision 3).
func TestConformantActionIsClean(t *testing.T) {
	src := `use capabilities.shell.{ script }
@description("Clone a repo working tree at the requested ref.")
action cloneRepoAtVersion {
  args {
    workdir string @required
    ref     string @required
  }
  capability script(script: "deploy.cloneRepo", workdir: args.workdir, ref: args.ref)
}`
	if fs := CheckFile("dsl/deployment/actions.memql", src, nil); len(fs) != 0 {
		t.Fatalf("conformant action must be clean; got %v", rules(fs))
	}
}

// I7 acceptance: an action that calls a logic produces a finding (an action is
// a single external capability and may not invoke other constructs).
func TestActionCallingLogicIsFlagged(t *testing.T) {
	src := `use deployment.logic.{ nextSemver }
use capabilities.shell.{ script }
action deployBad {
  args { ref string @required }
  capability script(cmd: nextSemver(args.ref))
}`
	fs := CheckFile("dsl/deployment/actions.memql", src, nil)
	if !has(fs, "action-no-calls") {
		t.Fatalf("expected action-no-calls finding (action calls a logic); got %v", rules(fs))
	}
}

// An action that calls a mutation is flagged (it may not touch the graph via a
// mutation either).
func TestActionCallingMutationIsFlagged(t *testing.T) {
	src := `use cluster.mutations.{ createDeployment }
use capabilities.shell.{ script }
action recordBad {
  args { id string @required }
  capability script(cmd: createDeployment(id: args.id))
}`
	fs := CheckFile("dsl/deployment/actions.memql", src, nil)
	if !has(fs, "action-no-calls") {
		t.Fatalf("expected action-no-calls finding (action calls a mutation); got %v", rules(fs))
	}
}

// I7 acceptance: an action declaring more than one capability call produces a
// finding (exactly one external capability per action).
func TestActionWithTwoCapabilitiesIsFlagged(t *testing.T) {
	src := `use capabilities.shell.{ script }
use capabilities.fs.{ writeFile }
action twoThingsBad {
  args { }
  capability script(script: "x")
  capability writeFile(path: "y")
}`
	fs := CheckFile("dsl/deployment/actions.memql", src, nil)
	if !has(fs, "action-one-capability") {
		t.Fatalf("expected action-one-capability finding (two capabilities); got %v", rules(fs))
	}
}

// I7 acceptance: an action that performs a graph write block is flagged (graph
// writes are mutations; an action never reaches the graph).
func TestActionTouchingGraphIsFlagged(t *testing.T) {
	src := `use capabilities.shell.{ script }
action writeBad {
  args { id string @required }
  capability script(script: "x")
  insert { id: args.id }
}`
	fs := CheckFile("dsl/deployment/actions.memql", src, nil)
	if !has(fs, "action-no-graph") {
		t.Fatalf("expected action-no-graph finding (graph write); got %v", rules(fs))
	}
}

// An action carrying a @trigger is flagged -- reactivity is automations-only
// (the shared trigger-monopoly rule, ADR §2.4).
func TestTriggeredActionIsFlagged(t *testing.T) {
	src := `use capabilities.shell.{ script }
@trigger(event="deploy.requested")
action onDeployBad {
  args { }
  capability script(script: "x")
}`
	fs := CheckFile("dsl/deployment/actions.memql", src, nil)
	if !has(fs, "trigger-monopoly") {
		t.Fatalf("expected trigger-monopoly finding (action carries @trigger); got %v", rules(fs))
	}
}

// An action whose capability resolves to a namespace outside the vocabulary is
// flagged.
func TestActionWithUnknownNamespaceIsFlagged(t *testing.T) {
	src := `use capabilities.weird.{ doThing }
action weirdBad {
  args { }
  capability doThing(x: "y")
}`
	fs := CheckFile("dsl/deployment/actions.memql", src, nil)
	if !has(fs, "action-capability-namespace") {
		t.Fatalf("expected action-capability-namespace finding; got %v", rules(fs))
	}
}

// The tree walker recognises actions.memql files as the action kind.
func TestActionsFileIsAnalysed(t *testing.T) {
	src := `use capabilities.shell.{ script }
action a {
  args { }
  capability script(script: "x")
  capability script(script: "y")
}`
	if fs := CheckFile("dsl/deployment/actions.memql", src, nil); !has(fs, "action-one-capability") {
		t.Fatalf("expected actions.memql to be analysed as the action kind; got %v", rules(fs))
	}
	// A non-action file name is not analysed as an action.
	if fs := CheckFile("dsl/deployment/concepts.memql", src, nil); len(fs) != 0 {
		t.Fatalf("non-restricted file must yield no findings; got %v", rules(fs))
	}
}
