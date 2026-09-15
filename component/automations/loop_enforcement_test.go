package automations

import (
	"strings"
	"testing"
)

func TestLoopGraphDisabledCycle(t *testing.T) {
	a := graphAutomation(t, selfUpdating)
	disabled := false
	a.Enabled = &disabled
	if g := BuildLoopGraph([]*Automation{a}, NewFunctionSource(forgeFunctions(t)), 0); len(g.Problems) != 0 || len(g.Automations) != 0 {
		t.Fatalf("disabled automation is scheduled in graph: %+v", g)
	}
	a.Enabled = nil
	if g := BuildLoopGraph([]*Automation{a}, NewFunctionSource(forgeFunctions(t)), 0); len(g.Problems) == 0 {
		t.Fatal("enabled self-cycle was admitted")
	}
}

func TestAuthoredDisabledStillChecked(t *testing.T) {
	s := authoredForgeScheduler(t, nil, true)
	if err := s.Activate(authoredLoopConstruct("reAdvance", "@disabled\n"+selfUpdating)); err == nil || !strings.Contains(err.Error(), "[loop_cycle]") {
		t.Fatalf("disabled authored cycle: %v", err)
	}
}

func TestAuthoredCycleThroughActiveCandidate(t *testing.T) {
	s := authoredForgeScheduler(t, nil, true)
	s.shipped = nil
	a := `@trigger(event="loop.a")
automation first {
  publish "loop.b" { value: 1 }
}`
	b := `@trigger(event="loop.b")
automation second {
  publish "loop.a" { value: 1 }
}`
	if err := s.Activate(authoredLoopConstruct("first", a)); err != nil {
		t.Fatal(err)
	}
	if err := s.Activate(authoredLoopConstruct("second", b)); err == nil || !strings.Contains(err.Error(), "[loop_cycle]") {
		t.Fatalf("mutual authored cycle: %v", err)
	}
	// Replacing first must remove the old version from the judged graph.
	a = strings.Replace(a, `"loop.b"`, `"loop.c"`, 1)
	if err := s.Activate(authoredLoopConstruct("first", a)); err != nil {
		t.Fatal(err)
	}
	if err := s.Activate(authoredLoopConstruct("second", b)); err != nil {
		t.Fatalf("old version retained: %v", err)
	}
}

func TestLoopCoveredMemberDoesNotCoverOthersSelfEdge(t *testing.T) {
	as := graphAutomations(t, `@trigger(event="a")
automation a {
  publish "b" { value: 1 }
}`, `@trigger(event="b")
automation b {
  publish "a" { value: 1 }
  publish "b" { value: 1 }
}`)
	as[0] = withLoop(as[0])
	if g := BuildLoopGraph(as, nil, 0); len(g.Problems) == 0 {
		t.Fatal("b's self edge escaped through a covered SCC member")
	}
}
