package callgraph

import "testing"

// A conformant action -- one external capability, no construct calls, no graph
// write, no trigger -- is clean.
func TestConformantActionIsClean(t *testing.T) {
	src := `@kind("primitive")
@sideEffect("exec")
action cloneRepoAtVersion {
  capability "shell.exec"
  intent "Clone a repo working tree at the requested ref."
  params {
    workdir string @required
    ref     string @required
  }
  argTemplate {
    cmd: "git -C $params.workdir checkout $params.ref"
  }
}`
	if fs := CheckFile("dsl/deployment/actions.memql", src, nil); len(fs) != 0 {
		t.Fatalf("conformant action must be clean; got %v", rules(fs))
	}
}

// I7 acceptance: an action that calls a logic produces a finding (an action is
// a single external capability and may not invoke other constructs).
func TestActionCallingLogicIsFlagged(t *testing.T) {
	src := `use deployment.logic.{ nextSemver }
@sideEffect("exec")
action deployBad {
  capability "shell.exec"
  intent "..."
  params { ref string @required }
  argTemplate { cmd: nextSemver(args.ref) }
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
action recordBad {
  capability "shell.exec"
  intent "..."
  params { id string @required }
  argTemplate { cmd: createDeployment(args.id) }
}`
	fs := CheckFile("dsl/deployment/actions.memql", src, nil)
	if !has(fs, "action-no-calls") {
		t.Fatalf("expected action-no-calls finding (action calls a mutation); got %v", rules(fs))
	}
}

// I7 acceptance: an action declaring more than one capability produces a
// finding (exactly one external capability per action).
func TestActionWithTwoCapabilitiesIsFlagged(t *testing.T) {
	src := `action twoThingsBad {
  capability "shell.exec"
  capability "fs.writeFile"
  intent "..."
  params {}
  argTemplate {}
}`
	fs := CheckFile("dsl/deployment/actions.memql", src, nil)
	if !has(fs, "action-one-capability") {
		t.Fatalf("expected action-one-capability finding (two capabilities); got %v", rules(fs))
	}
}

// I7 acceptance: an action that performs a graph write block is flagged (graph
// writes are mutations; an action never reaches the graph).
func TestActionTouchingGraphIsFlagged(t *testing.T) {
	src := `action writeBad {
  capability "shell.exec"
  intent "..."
  params { id string @required }
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
	src := `@trigger(event="deploy.requested")
action onDeployBad {
  capability "shell.exec"
  intent "..."
  params {}
  argTemplate {}
}`
	fs := CheckFile("dsl/deployment/actions.memql", src, nil)
	if !has(fs, "trigger-monopoly") {
		t.Fatalf("expected trigger-monopoly finding (action carries @trigger); got %v", rules(fs))
	}
}

// An action whose capability is outside the namespace vocabulary is flagged.
func TestActionWithUnknownNamespaceIsFlagged(t *testing.T) {
	src := `action weirdBad {
  capability "weird.doThing"
  intent "..."
  params {}
  argTemplate {}
}`
	fs := CheckFile("dsl/deployment/actions.memql", src, nil)
	if !has(fs, "action-capability-namespace") {
		t.Fatalf("expected action-capability-namespace finding; got %v", rules(fs))
	}
}

// The tree walker recognises actions.memql files as the action kind.
func TestActionsFileIsAnalysed(t *testing.T) {
	src := `action a {
  capability "shell.exec"
  capability "shell.exec"
  intent "..."
  params {}
  argTemplate {}
}`
	if fs := CheckFile("dsl/deployment/actions.memql", src, nil); !has(fs, "action-one-capability") {
		t.Fatalf("expected actions.memql to be analysed as the action kind; got %v", rules(fs))
	}
	// A non-action file name is not analysed as an action.
	if fs := CheckFile("dsl/deployment/concepts.memql", src, nil); len(fs) != 0 {
		t.Fatalf("non-restricted file must yield no findings; got %v", rules(fs))
	}
}
