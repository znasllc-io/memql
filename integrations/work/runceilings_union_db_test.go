package work

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

func TestRunUsageUnionDB_WarmReplicaCountsPeerReceipts(t *testing.T) {
	// Summary pagination must stay complete even below the normal 50-row page.
	t.Setenv("MEMQL_MEMORY_ENGINE_MAX_RESULTS", "3")
	a, b := openWorkTestEngine(t), openWorkTestEngine(t)
	owner := "dbtest-run-usage-" + time.Now().UTC().Format("20060102150405.000000000")
	ctx := actorCtx(owner)
	s := New(a, testLogger()).store()
	goal, run := newRowId(goalConcept), newRowId(runConcept)
	if err := s.createGoalRow(ctx, goalSeed{GoalId: goal, Statement: "Account for completed requests", Origin: "user", Ceilings: map[string]any{"maxModelCalls": 48}}); err != nil {
		t.Fatal(err)
	}
	if err := s.createRunRow(ctx, runSeed{RunId: run, GoalId: goal, AutomationName: "usageProbe", TemplateFingerprint: "usage-probe", Status: runStatusRunning, Mode: modeLive, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	rc := common.RunContext{RunId: run, GoalId: goal, OwnerUserId: owner, Mode: common.RunModeLive, StepKey: "work"}
	ga, gb := NewRunCeilings(a, testLogger()), NewRunCeilings(b, testLogger())
	ja, jb := NewModelJournal(a), NewModelJournal(b)
	record := func(j *ModelJournal, g *RunCeilings, id string, tokens int) {
		t.Helper()
		id = memqlengine.BareShortId(run) + "-" + id
		call := memqlengine.JournaledCall{ReceiptID: id, RunId: run, StepKey: "work", RequestHash: id, PromptRef: "usageProbe", Served: "local", InputTokens: tokens}
		if g != nil {
			g.Charge(ctx, rc, memqlengine.ModelSpend{ReceiptID: id, Served: "local", InputTokens: tokens})
		}
		if err := j.Record(ctx, owner, call); err != nil {
			t.Fatal(err)
		}
	}
	// This reproduces the observed partition: 40 prior, 8 on this warm replica,
	// 8 on a peer. Calls are recorded directly to exercise recovery of an already
	// overspent journal; this fixture does not claim in-flight admission isolation.
	for n := 0; n < 40; n++ {
		tokens := 7990
		if n == 39 {
			tokens = 7899
		}
		record(ja, nil, fmt.Sprintf("seed-%d", n), tokens)
	}
	if breach := ga.Admit(ctx, rc, 0); breach != nil {
		t.Fatal(breach)
	}
	if got := ga.Spent(ctx, rc); got.ModelCalls != 40 || got.TokensLocal != 319509 {
		t.Fatalf("seed=%+v", got)
	}
	for n := 0; n < 4; n++ {
		record(ja, ga, fmt.Sprintf("own-%d", n), 8000)
	}
	if breach := gb.Admit(ctx, rc, 0); breach != nil {
		t.Fatal(breach)
	}
	// Prime A's ordinary summary result before B writes. Accounting must bypass
	// that cache when the work returns to the warm replica.
	if _, err := a.Execute(ctx, "query "+call("workModelCallsPageForOwnerRun", map[string]any{"runId": run})); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 8; n++ {
		tokens := 8700
		if n == 7 {
			tokens = 9203
		}
		record(jb, gb, fmt.Sprintf("peer-%d", n), tokens)
	}
	for n := 4; n < 8; n++ {
		tokens := 8000
		if n == 7 {
			tokens = 8222
		}
		record(ja, ga, fmt.Sprintf("own-%d", n), tokens)
	}
	for n := 0; n < 3; n++ {
		breach := ga.Admit(ctx, rc, 0)
		if breach == nil || breach.Ceiling != work.CeilingModelCalls {
			t.Fatalf("warm A did not refuse: %+v", breach)
		}
		got := ga.Spent(ctx, rc)
		if got.ModelCalls != 56 || got.TokensLocal != 453834 {
			t.Fatalf("warm A lost peer spend or doubled own receipts: %+v; want 56/453834", got)
		}
	}
	// A cold third reader sees exactly the same immutable receipts.
	gc := NewRunCeilings(b, testLogger())
	if breach := gc.Admit(ctx, rc, 0); breach == nil {
		t.Fatal("cold replica admitted overspent run")
	}
	if ga.Spent(ctx, rc) != gc.Spent(ctx, rc) { // Wall time is measured at read time.
		x, y := ga.Spent(ctx, rc), gc.Spent(ctx, rc)
		x.WallClockMs = 0
		y.WallClockMs = 0
		if x != y {
			t.Fatalf("warm/cold differ: %+v %+v", x, y)
		}
	}
}

func TestRunUsageUnionDB_AdmissionFollowsAToBToAAndKeepsBillingBuckets(t *testing.T) {
	a, b := openWorkTestEngine(t), openWorkTestEngine(t)
	owner := "dbtest-run-buckets-" + time.Now().UTC().Format("20060102150405.000000000")
	ctx := actorCtx(owner)
	s := New(a, testLogger()).store()
	goal, run := newRowId(goalConcept), newRowId(runConcept)
	if err := s.createGoalRow(ctx, goalSeed{GoalId: goal, Statement: "Count each answer once", Origin: "user", Ceilings: map[string]any{"maxModelCalls": 3}}); err != nil {
		t.Fatal(err)
	}
	if err := s.createRunRow(ctx, runSeed{RunId: run, GoalId: goal, AutomationName: "usageProbe", TemplateFingerprint: "usage-probe", Status: runStatusRunning, Mode: modeLive, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	rc := common.RunContext{RunId: run, GoalId: goal, OwnerUserId: owner, Mode: common.RunModeLive}
	ga, gb := NewRunCeilings(a, testLogger()), NewRunCeilings(b, testLogger())
	for n, item := range []struct {
		g      *RunCeilings
		e      Engine
		served string
		tokens int
	}{{ga, a, "live", 10}, {gb, b, "subscription", 20}, {ga, a, "local", 30}} {
		if breach := item.g.Admit(ctx, rc, 0); breach != nil {
			t.Fatalf("call %d refused: %+v", n, breach)
		}
		id := memqlengine.BareShortId(run) + fmt.Sprintf("-bucket-%d", n)
		item.g.Charge(ctx, rc, memqlengine.ModelSpend{ReceiptID: id, Served: item.served, InputTokens: item.tokens})
		j := NewModelJournal(item.e)
		if err := j.Record(ctx, owner, memqlengine.JournaledCall{ReceiptID: id, RunId: run, StepKey: "work", RequestHash: id, Served: item.served, InputTokens: item.tokens}); err != nil {
			t.Fatal(err)
		}
	}
	for _, g := range []*RunCeilings{ga, gb} {
		if breach := g.Admit(ctx, rc, 0); breach == nil || breach.Ceiling != work.CeilingModelCalls {
			t.Fatalf("fourth call admitted: %+v", breach)
		}
		got := g.Spent(ctx, rc)
		if got.ModelCalls != 3 || got.Tokens != 10 || got.TokensSubscription != 20 || got.TokensLocal != 30 {
			t.Fatalf("bucket union=%+v", got)
		}
	}
	// Preserve an answered call whose actual journal writer fails, plus a cache
	// hit with no row. Later peer receipts must add to both, never replace them.
	failedID := memqlengine.BareShortId(run) + "-failed-write"
	ga.Charge(ctx, rc, memqlengine.ModelSpend{ReceiptID: failedID, Served: "local", InputTokens: 40})
	failedCall := memqlengine.JournaledCall{ReceiptID: failedID, RunId: run, StepKey: "work", RequestHash: failedID, Served: "local", InputTokens: 40}
	if err := NewModelJournal(usageWriteFailureEngine{a}).Record(ctx, owner, failedCall); err == nil {
		t.Fatal("receipt write did not fail")
	}
	ga.Charge(ctx, rc, memqlengine.ModelSpend{Served: "cache"})
	peerCall := memqlengine.JournaledCall{ReceiptID: memqlengine.BareShortId(run) + "-later-peer", RunId: run, StepKey: "work", RequestHash: "later-peer", Served: "subscription", InputTokens: 50}
	if err := NewModelJournal(b).Record(ctx, owner, peerCall); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		if breach := ga.Admit(ctx, rc, 0); breach == nil {
			t.Fatal("over-budget run admitted")
		}
		got := ga.Spent(ctx, rc)
		if got.ModelCalls != 6 || got.Tokens != 10 || got.TokensSubscription != 70 || got.TokensLocal != 70 {
			t.Fatalf("failed write/cache/peer union=%+v", got)
		}
		// The failed write may later succeed with its ORIGINAL identity.
		if n == 0 {
			if err := NewModelJournal(a).Record(ctx, owner, failedCall); err != nil {
				t.Fatal(err)
			}
		}
	}
}

type usageWriteFailureEngine struct{ Engine }

func (e usageWriteFailureEngine) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	if strings.Contains(q, "createWorkModelCall(") {
		return nil, fmt.Errorf("injected receipt write failure")
	}
	return e.Engine.Execute(ctx, q)
}
