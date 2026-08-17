package harness

import (
	"context"
	"strings"
	"testing"

	"github.com/uptrace/bun"
)

// staged_read_gate_3984_test.go -- epic memql#3974, task memql#3984.
//
// These reads are hand-rolled bun statements, so nothing memql#3983 injects at
// the engine's parse or filter seam reaches them: the check written into each
// function IS the enforcement. This file proves the check runs, and it does so
// WITHOUT A DATABASE on purpose.
//
// The construction is the point. Each reader is built with a NIL bun handle and
// a predicate that says "staged". If the gate runs, the function returns its
// empty answer before it ever touches the handle. If the gate were deleted, the
// same call would reach `r.handle()` and fail -- so the test cannot pass against
// an ungated function, which is what makes it evidence rather than decoration.
// It is also why the gates sit BEFORE the "database not configured" check in
// each function.

func stagedAlways(string) bool { return true }
func stagedNever(string) bool  { return false }
func nilDB() *bun.DB           { return nil }

// TestStepReaderWithholdsStagedRows: every read on BunStepReader answers empty
// when its concept's data is staged, and answers it without a database.
func TestStepReaderWithholdsStagedRows(t *testing.T) {
	ctx := context.Background()
	r := NewBunStepReader(nilDB, stagedAlways)

	steps, partition, err := r.StepsForPlan(ctx, "v1:harness:plan:abc")
	if err != nil || len(steps) != 0 || partition != "" {
		t.Errorf("StepsForPlan = (%d steps, %q, %v), want (0, \"\", nil) -- a staged step must not "+
			"reach the reconciler, which would claim and dispatch it", len(steps), partition, err)
	}

	status, err := r.PlanStatus(ctx, "v1:harness:plan:abc")
	if err != nil || status != "" {
		t.Errorf("PlanStatus = (%q, %v), want (\"\", nil)", status, err)
	}

	view, found, err := r.PlanView(ctx, "v1:harness:plan:abc")
	if err != nil || found {
		t.Errorf("PlanView = (%+v, found=%v, %v), want (zero, false, nil) -- a staged plan has to be "+
			"indistinguishable from one that was never written", view, found, err)
	}
}

// TestAgentRosterWithholdsStagedRows: the roster returns no agents when the
// agent concept's data is staged. This is the read with no owner predicate in
// SQL at all, so an ungated one hands the planner every agent row in the
// cluster.
func TestAgentRosterWithholdsStagedRows(t *testing.T) {
	roster := NewBunAgentRoster(nilDB, nil, stagedAlways)
	agents, err := roster.ListAgents(context.Background(), "user-1")
	if err != nil || len(agents) != 0 {
		t.Errorf("ListAgents = (%d agents, %v), want (0, nil) -- a staged agent must not become "+
			"selectable and dispatchable", len(agents), err)
	}
}

// TestHarnessReadsRefuseWhenTheStagedPredicateIsMissing: an unwired predicate
// is a WIRING failure and must be refused, never resolved to "nothing is
// staged".
//
// This is the half that decides what happens on a mistake nobody made
// deliberately. A nil-tolerant default would make a node that cannot answer the
// question answer it "no", serve staged rows, and look completely healthy doing
// it -- and the mistake that produced it (a constructor call that dropped an
// argument, a plug-in factory that forgot a line) is exactly the kind that
// reaches production unnoticed.
func TestHarnessReadsRefuseWhenTheStagedPredicateIsMissing(t *testing.T) {
	ctx := context.Background()
	r := NewBunStepReader(nilDB, nil)

	if _, _, err := r.StepsForPlan(ctx, "p"); err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Errorf("StepsForPlan with no predicate = %v, want a refusal naming the unwired predicate", err)
	}
	if _, err := r.PlanStatus(ctx, "p"); err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Errorf("PlanStatus with no predicate = %v, want a refusal", err)
	}
	if _, _, err := r.PlanView(ctx, "p"); err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Errorf("PlanView with no predicate = %v, want a refusal", err)
	}

	roster := NewBunAgentRoster(nilDB, nil, nil)
	if _, err := roster.ListAgents(ctx, "u"); err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Errorf("ListAgents with no predicate = %v, want a refusal", err)
	}
}

// TestHarnessReadsProceedWhenNothingIsStaged: the gate is not a blanket
// refusal.
//
// The control for the two tests above, and it is not padding: a "gate" that
// returned empty unconditionally would pass both of them. Here the predicate
// says nothing is staged, so each read must get PAST the gate -- observable as
// the database error it then hits, which is the next thing in the function.
func TestHarnessReadsProceedWhenNothingIsStaged(t *testing.T) {
	ctx := context.Background()
	r := NewBunStepReader(nilDB, stagedNever)

	_, _, err := r.StepsForPlan(ctx, "p")
	if err == nil || !strings.Contains(err.Error(), "database not configured") {
		t.Errorf("StepsForPlan with nothing staged = %v, want it to reach the database check -- a gate "+
			"that withholds unconditionally is not a gate", err)
	}

	roster := NewBunAgentRoster(nilDB, nil, stagedNever)
	if _, err := roster.ListAgents(ctx, "u"); err == nil || !strings.Contains(err.Error(), "database not configured") {
		t.Errorf("ListAgents with nothing staged = %v, want it to reach the database check", err)
	}
}
