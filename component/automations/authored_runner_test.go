package automations_test

// authored_runner_test.go -- one event runs an authored automation once, on
// whichever replicas observe it locally (memql#6022, F2).
//
// An event trigger fires on the node where its event happened. That rule is
// the primary guarantee; this is the backstop behind it: the run for an EVENT
// is claimed per (automation, event) -- the same machinery the core
// scheduler's event executor uses -- so an event that is somehow observed as
// local on two replicas still runs the automation once. A scheduled run has
// no event to claim and keeps running every time it is due.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
)

// sharedClaims stands in for automation_execution_claims: one table every
// replica claims against, one winner per (automation, key).
type sharedClaims struct {
	mu    sync.Mutex
	won   map[string]bool
	asked int
}

func (c *sharedClaims) Claim(_ context.Context, automation, key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked++
	if c.won == nil {
		c.won = map[string]bool{}
	}
	if c.won[automation+"\x00"+key] {
		return false
	}
	c.won[automation+"\x00"+key] = true
	return true
}

func (c *sharedClaims) consulted() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.asked
}

// stepCounter counts the steps that actually ran: the difference between a
// firing that ran and one the claim skipped.
type stepCounter struct {
	mu  sync.Mutex
	ran int
}

func (s *stepCounter) Execute(_ context.Context, step *automations.Step, _ *automations.StepContext) (*automations.StepResult, error) {
	s.mu.Lock()
	s.ran++
	s.mu.Unlock()
	now := time.Now()
	return &automations.StepResult{StepId: step.ID, Status: "completed", StartedAt: now, CompletedAt: now}, nil
}

func (s *stepCounter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ran
}

func authoredRuleWithOneStep() *automations.Automation {
	return &automations.Automation{
		Name:   "emailRuleClaimed",
		Origin: "authored:" + originOwner + ":emailRuleClaimed",
		Steps: []*automations.Step{{
			ID: "fire", Type: automations.StepTypeFunction,
			Function: &automations.FunctionStepConfig{Name: "emailRuleFire", Kind: "builtin"},
		}},
	}
}

func todoCreated(id string) *events.Event {
	ev := events.NewEvent("graph.node.created.v1:todos:todo", events.KindNodeCreated,
		map[string]any{"id": id, "concept": "v1:todos:todo", "firstVersion": true})
	return &ev
}

// TestOneEventRunsAnAuthoredAutomationOnce: the same event delivered twice on
// one node, and once more on a second node claiming against the same table,
// runs the automation's step once. A different event runs it again.
func TestOneEventRunsAnAuthoredAutomationOnce(t *testing.T) {
	ctx := automations.AuthorContext(context.Background(), originOwner)
	claims := &sharedClaims{}
	steps := &stepCounter{}
	nodeA := automations.NewAuthoredRunner(automations.AuthoredRunnerOptions{StepRegistry: steps, ClusterGuard: claims})
	nodeB := automations.NewAuthoredRunner(automations.AuthoredRunnerOptions{StepRegistry: steps, ClusterGuard: claims})
	rule := authoredRuleWithOneStep()

	ev := todoCreated("v1:todos:todo:t1")
	for i := 0; i < 2; i++ {
		if err := nodeA(ctx, rule, ev); err != nil {
			t.Fatalf("run %d on node A: %v", i+1, err)
		}
	}
	if steps.count() != 1 {
		t.Fatalf("one event delivered twice on one node ran the rule %d times, want 1", steps.count())
	}

	if err := nodeB(ctx, rule, ev); err != nil {
		t.Fatalf("run on node B: %v", err)
	}
	if steps.count() != 1 {
		t.Fatalf("the same event observed on a second node ran the rule again (%d runs); "+
			"the claim is what makes one event one run whatever node sees it", steps.count())
	}
	if claims.consulted() == 0 {
		t.Fatal("the cross-replica claim was never consulted, so node B was stopped by something else")
	}

	if err := nodeB(ctx, rule, todoCreated("v1:todos:todo:t2")); err != nil {
		t.Fatalf("a second event on node B: %v", err)
	}
	if steps.count() != 2 {
		t.Fatalf("a different event ran the rule %d times in all, want 2", steps.count())
	}
}

// TestAScheduledAuthoredRunIsNeverClaimed: a scheduled firing has no event, so
// there is nothing to claim it by; it runs every time it is due, as before.
func TestAScheduledAuthoredRunIsNeverClaimed(t *testing.T) {
	ctx := automations.AuthorContext(context.Background(), originOwner)
	claims := &sharedClaims{}
	steps := &stepCounter{}
	run := automations.NewAuthoredRunner(automations.AuthoredRunnerOptions{StepRegistry: steps, ClusterGuard: claims})
	rule := authoredRuleWithOneStep()

	for i := 0; i < 3; i++ {
		if err := run(ctx, rule, nil); err != nil {
			t.Fatalf("scheduled run %d: %v", i+1, err)
		}
	}
	if steps.count() != 3 {
		t.Fatalf("three scheduled firings ran the rule %d times, want 3", steps.count())
	}
	if n := claims.consulted(); n != 0 {
		t.Fatalf("a scheduled run consulted the event claim %d times", n)
	}
}
