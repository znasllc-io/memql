package work

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

func usageRow(id, served string, tokens int) map[string]any {
	return map[string]any{"id": id, "createdAt": rfc(testNow), "ownerUserId": ceilingOwner, "runId": ceilingRunId, "served": served, "inputTokens": tokens}
}

type longUsagePageEngine struct {
	Engine
	count int
}

func (e longUsagePageEngine) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	if !strings.Contains(q, "workModelCallsPageForOwnerRun") {
		return e.Engine.Execute(ctx, q)
	}
	start := 0
	if cursor := memqlengine.CursorFromContext(ctx); cursor != "" {
		var err error
		start, err = strconv.Atoi(cursor)
		if err != nil {
			return nil, err
		}
	}
	end := min(start+50, e.count)
	var rows []any
	for n := start; n < end; n++ {
		rows = append(rows, usageRow(fmt.Sprintf("receipt-%08d", n), "local", 1))
	}
	result := memqlengine.NewResultWithOutput(rows)
	if end < e.count {
		result.SetCursor(strconv.Itoa(end))
		result.SetHasMore(true)
	}
	return result, nil
}

func TestRunUsageLifetimeHistoryDoesNotFillThePendingReceiptIndex(t *testing.T) {
	c, eng := newTestCeilings(t)
	answerRows(eng, nil)
	c.store.engine = longUsagePageEngine{Engine: eng, count: 5000}
	rc := aGoalBackedRun()
	// Explicitly exercise accounting without claiming that 5,000 calls fit the
	// current work window. That policy check remains independently enforced.
	b := c.budgetFor(rc)
	for n := 0; n < 2; n++ {
		if err := c.refreshReceipts(context.Background(), rc, b); err != nil {
			t.Fatal(err)
		}
	}
	if b.spent.ModelCalls != 5000 || b.spent.TokensLocal != 5000 {
		t.Fatalf("incomplete cumulative usage: %+v", b.spent)
	}
	if len(b.receipts) != 0 || len(b.pending) != 0 {
		t.Fatal("durable history filled the active pending index")
	}
}

func TestRunUsageUnionKeepsLocalOnlyChargesAndNeverRefundsObservedReceipts(t *testing.T) {
	c, eng := newTestCeilings(t)
	answerRows(eng, map[string]any{"maxModelCalls": 20})
	ctx, rc := context.Background(), aGoalBackedRun()
	if b := c.Admit(ctx, rc, 0); b != nil {
		t.Fatal(b)
	}
	c.Charge(ctx, rc, memqlengine.ModelSpend{ReceiptID: "local", Served: "local", InputTokens: 100})
	// Its durable write failed. The unjournaled cache charge must survive too.
	c.Charge(ctx, rc, memqlengine.ModelSpend{Served: "cache"})
	eng.reply("workModelCallsPageForOwnerRun", usageRow("peer", "subscription", 200))
	for n := 0; n < 3; n++ {
		if b := c.Admit(ctx, rc, 0); b != nil {
			t.Fatal(b)
		}
	}
	got := c.Spent(ctx, rc)
	if got.ModelCalls != 3 || got.TokensLocal != 100 || got.TokensSubscription != 200 || got.Tokens != 0 {
		t.Fatalf("union lost or duplicated spend: %+v", got)
	}
	// A later successful retry of the same receipt cannot charge twice.
	eng.reply("workModelCallsPageForOwnerRun", usageRow("peer", "subscription", 200), usageRow("local", "local", 100))
	eng.reply("workModelCallReceiptForOwnerRun", usageRow("local", "local", 100))
	if b := c.Admit(ctx, rc, 0); b != nil {
		t.Fatal(b)
	}
	c.Charge(ctx, rc, memqlengine.ModelSpend{ReceiptID: "local", Served: "local", InputTokens: 100})
	if c.Spent(ctx, rc) != got {
		t.Fatalf("same receipt charged twice: %+v", c.Spent(ctx, rc))
	}
	// Even a genuinely removed receipt is not a budget refund.
	eng.reply("workModelCallsPageForOwnerRun")
	if b := c.Admit(ctx, rc, 0); b == nil || b.Ceiling != work.CeilingUnevaluated {
		t.Fatalf("a regressed durable journal must require accounting recovery: %+v", b)
	}
	if c.Spent(ctx, rc) != got {
		t.Fatalf("missing receipt refunded usage: %+v", c.Spent(ctx, rc))
	}
	eng.refuse("workModelCallsPageForOwnerRun", errors.New("summary unavailable"))
	if b := c.Admit(ctx, rc, 0); b == nil || b.Ceiling != work.CeilingUnevaluated {
		t.Fatalf("warm read failure admitted: %+v", b)
	}
	if c.Spent(ctx, rc) != got {
		t.Fatal("failed read changed existing spend")
	}
}

func TestRunUsageUnionSeparatesOwnersAndRejectsForeignReceipt(t *testing.T) {
	c, eng := newTestCeilings(t)
	answerRows(eng, map[string]any{"maxModelCalls": 10})
	ctx, rc := context.Background(), aGoalBackedRun()
	if b := c.Admit(ctx, rc, 0); b != nil {
		t.Fatal(b)
	}
	other := rc
	other.OwnerUserId = "someone-else"
	eng.reply("workRunForOwner")
	if b := c.Admit(ctx, other, 0); b == nil || b.Ceiling != work.CeilingUnevaluated {
		t.Fatalf("different owner reused seed: %+v", b)
	}
	row := usageRow("foreign", "live", 900)
	row["ownerUserId"] = "someone-else"
	eng.reply("workModelCallsPageForOwnerRun", row)
	if b := c.Admit(ctx, rc, 0); b == nil || b.Ceiling != work.CeilingUnevaluated {
		t.Fatalf("foreign receipt admitted: %+v", b)
	}
	if c.Spent(ctx, rc).ModelCalls != 0 {
		t.Fatal("foreign spend entered owned run")
	}
}

func TestRunUsageUnionReceiptTrackingIsBoundedAndRefusesInsteadOfForgetting(t *testing.T) {
	c, eng := newTestCeilings(t)
	answerRows(eng, nil)
	ctx, rc := context.Background(), aGoalBackedRun()
	for n := 0; n < maxTrackedRunReceipts; n++ {
		c.Charge(ctx, rc, memqlengine.ModelSpend{ReceiptID: itoa(n), Served: "local", InputTokens: 1})
	}
	if b := c.Admit(ctx, rc, 0); b == nil || b.Ceiling != work.CeilingUnevaluated {
		t.Fatalf("tracking exhaustion admitted: %+v", b)
	}
	if len(c.budgetFor(rc).receipts) != maxTrackedRunReceipts {
		t.Fatal("unbounded receipt tracking")
	}
	if c.Spent(ctx, rc).ModelCalls != maxTrackedRunReceipts {
		t.Fatal("tracking exhaustion lost charges")
	}
}

type usagePageEngine struct {
	Engine
	failSecond bool
	reads      int
}

func (e *usagePageEngine) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	if !strings.Contains(q, "workModelCallsPageForOwnerRun") {
		return e.Engine.Execute(ctx, q)
	}
	e.reads++
	if !memqlengine.FreshReadFromContext(ctx) {
		return nil, errors.New("usage read used the result cache")
	}
	if callerUserId(ctx) != ceilingOwner {
		return nil, errors.New("usage read lost owner")
	}
	if memqlengine.CursorFromContext(ctx) == "" {
		result := memqlengine.NewResultWithOutput([]any{usageRow("first", "local", 10)})
		result.SetCursor("page-two")
		result.SetHasMore(true)
		return result, nil
	}
	if e.failSecond {
		return nil, errors.New("second page failed")
	}
	return memqlengine.NewResultWithOutput([]any{usageRow("second", "subscription", 20)}), nil
}

func TestRunUsageUnionPartialReadFailsClosedAndRetryDoesNotDoubleCount(t *testing.T) {
	c, eng := newTestCeilings(t)
	answerRows(eng, map[string]any{"maxModelCalls": 10})
	pager := &usagePageEngine{Engine: eng, failSecond: true}
	c.store.engine = pager
	ctx, rc := context.Background(), aGoalBackedRun()
	// The caller's cursor must not skip accounting's first page.
	ctx = memqlengine.ContextWithCursor(ctx, "unrelated-caller-page")
	if b := c.Admit(ctx, rc, 0); b == nil || b.Ceiling != work.CeilingUnevaluated {
		t.Fatalf("partial usage admitted: %+v", b)
	}
	pager.failSecond = false
	for n := 0; n < 2; n++ {
		if b := c.Admit(ctx, rc, 0); b != nil {
			t.Fatal(b)
		}
	}
	if got := c.Spent(ctx, rc); got.ModelCalls != 2 || got.TokensLocal != 10 || got.TokensSubscription != 20 {
		t.Fatalf("partial retry changed exact union: %+v", got)
	}
	if pager.reads != 6 {
		t.Fatalf("did not page all three fresh walks: %d", pager.reads)
	}
}
