package work

// Goal admission separates monetary limits from renewable work windows. Every
// answered model call counts, including local, subscription and replay calls;
// only metered tokens/cost consume monetary ceilings. Before a new call, fresh
// owner-scoped receipts reconcile local charges without double-counting peers.
//
// Time/model-call windows renew only through ContinueWorkProgress, which verifies
// new durable evidence or a human answer. No fixed goal-age deadline may cancel
// productive work. Provider/tool deadlines, cancellation, permission expiry and
// the stalled-loop detector remain independent. Retry counters belong to the
// executor; maxEvents is still warned as unenforced, never silently certified.
//
// A missing goal, unreadable usage or identity overflow fails closed rather than
// inventing budget. Local unjournaled answers remain counted on this replica;
// actual provider attempts have their separate durable reservation ledger.
// The active pending index is bounded separately from goal lifetime, and an
// exhausted set needs accounting recovery,
// not a claim that the owner's objective was accepted or completed.

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// maxTrackedRuns bounds the per-process counter table. A run evicted from it
// re-seeds from its own modelCall rows on the next call, so eviction costs a
// read rather than an allowance.
const maxTrackedRuns = 2048

// Bound unjournaled local charges, not a run's lifetime. Confirmed receipts
// leave this active index only after a complete durable accounting read.
const maxTrackedRunReceipts = 4096

// RunCeilings implements memqlengine.RunCeilingGuard over v1:work:goal's
// ceilings and v1:work:modelCall's rows.
type RunCeilings struct {
	store  *store
	logger *slog.Logger
	now    func() time.Time

	mu   sync.Mutex
	runs map[string]*runBudget
}

var _ memqlengine.RunCeilingGuard = (*RunCeilings)(nil)

// runBudget is one run's ceilings and its spend so far.
type runBudget struct {
	mu sync.Mutex
	// seeded is false until the rows have been read once.
	seeded          bool
	receipts        map[string]struct{}
	pending         map[string]work.CallSpend
	durable         work.Spent
	unrecorded      work.Spent
	receiptOverflow bool
	// ceilings are the goal's, with the two this seam cannot evaluate
	// cleared. unbounded is true when nothing is left to check.
	ceilings    work.Ceilings
	unbounded   bool
	spent       work.Spent
	startedAt   time.Time
	humanWaitMs int64
	touched     time.Time
}

// NewRunCeilings builds the guard over an engine. Returns nil for a nil
// engine, so app/ can wire unconditionally.
func NewRunCeilings(engine Engine, logger *slog.Logger) *RunCeilings {
	if engine == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &RunCeilings{
		store:  &store{engine: engine},
		logger: logger,
		now:    time.Now,
		runs:   map[string]*runBudget{},
	}
}

// Admit reports the ceiling this run has reached, or nil.
//
// A run with no goal on its context is admitted without a read: ceilings live
// on the goal, so there is nothing declared to check.
func (c *RunCeilings) Admit(ctx context.Context, rc common.RunContext, estimatedTokens int) *memqlengine.RunCeilingBreach {
	if c == nil || !rc.IsRun() || strings.TrimSpace(rc.GoalId) == "" {
		return nil
	}
	b := c.budgetFor(rc)
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := c.seed(ctx, rc, b); err != nil {
		// FAIL CLOSED AND SAY WHY. The seed is left unseeded so the next
		// call tries again: a blip costs one refused call, not the run.
		return breachOf(work.UnevaluatedBreach(
			"this run's ceilings could not be read, so whether it is within them is unknown: " + err.Error()))
	}
	if err := c.refreshReceipts(ctx, rc, b); err != nil {
		return breachOf(work.UnevaluatedBreach("this run’s model usage could not be reconciled: " + err.Error()))
	}
	if b.unbounded {
		return nil
	}
	spent := b.spent
	if !b.startedAt.IsZero() {
		spent.WallClockMs = max(int64(0), c.clock().Sub(b.startedAt).Milliseconds()-b.humanWaitMs)
	}
	breach := work.CheckCeilings(b.ceilings, spent, estimatedTokens)
	if breach == nil {
		return nil
	}

	// Re-read before parking: another replica may have persisted a progress
	// window since this replica seeded its allowance. Never reset spent usage.
	//
	// Usage was reconciled above by receipt identity, preserving local charges
	// even when their journal write failed or the answer came from a cache.
	//
	// The read only happens on the path that is about to STOP a run, so it
	// costs at most a handful per run rather than one per call. A read that
	// fails leaves the breach standing: we already have a real answer, and
	// replacing it with `unevaluated` would lose it.
	if declared, err := c.goalCeilings(ctx, rc); err == nil {
		b.ceilings = enforceableHere(declared)
		b.unbounded = b.ceilings == work.Ceilings{}
		if b.unbounded {
			return nil
		}
		breach = work.CheckCeilings(b.ceilings, spent, estimatedTokens)
	}
	if breach != nil && (breach.Ceiling == work.CeilingModelCalls || breach.Ceiling == work.CeilingWallClock) {
		if host, ok := c.store.engine.(interface {
			IntegrationByName(string) memqlengine.IntegrationProvider
		}); ok {
			if coordinator, ok := host.IntegrationByName("work").(*Integration); ok {
				continued, err := coordinator.ContinueWorkProgress(ctx, rc, spent, estimatedTokens)
				if err != nil {
					return breachOf(work.UnevaluatedBreach("work progress could not be verified: " + err.Error()))
				}
				if continued {
					if current, err := c.goalCeilings(ctx, rc); err == nil {
						b.ceilings = enforceableHere(current)
						breach = work.CheckCeilings(b.ceilings, spent, estimatedTokens)
					}
				}
			}
		}
	}
	return breachOf(breach)
}

// Charge records one answered request against the run.
//
// IT CHARGES EVEN WHEN THE RUN IS UNBOUNDED, because "unbounded" is a fact
// about this goal's ceilings and the figures are still what a reader sees on
// the park of a SIBLING run -- and because a ceiling raised mid-run would
// otherwise start counting from zero.
func (c *RunCeilings) Charge(ctx context.Context, rc common.RunContext, spend memqlengine.ModelSpend) {
	if c == nil || !rc.IsRun() || strings.TrimSpace(rc.GoalId) == "" {
		return
	}
	b := c.budgetFor(rc)
	b.mu.Lock()
	defer b.mu.Unlock()
	charge := work.CallSpend{Served: spend.Served, InputTokens: spend.InputTokens, OutputTokens: spend.OutputTokens, Cost: spend.Cost}
	if receiptID := memqlengine.BareShortId(strings.TrimSpace(spend.ReceiptID)); receiptID != "" {
		if _, pending := b.receipts[receiptID]; !pending {
			rows, err := c.store.query(ownerActor(memqlengine.ContextWithFreshRead(ctx), rc.OwnerUserId), "query "+call("workModelCallReceiptForOwnerRun", map[string]any{"runId": rc.RunId, "receiptId": receiptID}))
			if err == nil && len(rows) > 0 {
				// A late completion retry may name a receipt already included in
				// the durable total. Reconcile instead of charging it twice.
				if err := c.refreshReceipts(ctx, rc, b); err == nil {
					return
				}
			}
		}
		if err := b.addReceipt(receiptID, charge); err != nil {
			// An already-admitted answer must still count even after the bound was
			// reached. No further calls can be admitted with uncertain identity.
			b.receiptOverflow = true
			b.spent = work.AddCall(b.spent, charge)
		}
		return
	}
	// Cache hits have no durable row and therefore cannot overlap a fresh read.
	b.spent = work.AddCall(b.spent, charge)
	b.unrecorded = work.AddCall(b.unrecorded, charge)
}

// Spent reports the run's spend so far, for a refusal to carry onto the row.
func (c *RunCeilings) Spent(_ context.Context, rc common.RunContext) memqlengine.RunSpend {
	var out memqlengine.RunSpend
	if c == nil || !rc.IsRun() || strings.TrimSpace(rc.GoalId) == "" {
		return out
	}
	b := c.budgetFor(rc)
	b.mu.Lock()
	defer b.mu.Unlock()
	out = memqlengine.RunSpend{
		Tokens:             b.spent.Tokens,
		TokensSubscription: b.spent.TokensSubscription,
		TokensLocal:        b.spent.TokensLocal,
		Cost:               b.spent.Cost,
		ModelCalls:         b.spent.ModelCalls,
	}
	if !b.startedAt.IsZero() {
		out.WallClockMs = max(int64(0), c.clock().Sub(b.startedAt).Milliseconds()-b.humanWaitMs)
	}
	return out
}

// seed reads the run and its goal once. A seeded budget returns
// immediately.
func (c *RunCeilings) seed(ctx context.Context, rc common.RunContext, b *runBudget) error {
	if b.seeded {
		return nil
	}
	owner := strings.TrimSpace(rc.OwnerUserId)
	if owner == "" {
		// A goal is one person's, and its ceilings are read under their
		// borrowed authority. A run naming a goal with no owner on its
		// context is an anomaly, and reading the goal under whatever actor
		// happens to be in ctx would answer zero rows and no error -- which
		// presents as "this goal has no ceilings", unbounded.
		return fmt.Errorf("run %s names goal %s and carries no owner, so its ceilings cannot be read as anybody", rc.RunId, rc.GoalId)
	}
	scoped := ownerActor(memqlengine.ContextWithFreshRead(ctx), owner)

	run, err := c.store.runForOwner(scoped, rc.RunId)
	if err != nil {
		return fmt.Errorf("reading run %s: %w", rc.RunId, err)
	}
	if run == nil {
		return fmt.Errorf("run %s is not readable as %s", rc.RunId, owner)
	}
	if memqlengine.BareShortId(rowString(run, "goalId")) != memqlengine.BareShortId(rc.GoalId) {
		return fmt.Errorf("run does not belong to the named goal")
	}
	b.startedAt, _ = time.Parse(time.RFC3339Nano, rowString(run, "startedAt"))
	b.humanWaitMs = int64(rowInt(run, "humanWaitMs"))

	declared, err := c.goalCeilings(ctx, rc)
	if err != nil {
		return err
	}
	// Warned on the DECLARED set, before the two this seam cannot see are
	// cleared: after the clearing there is nothing left to warn about.
	c.warnAboutUnenforceable(rc, declared)
	b.ceilings = enforceableHere(declared)
	b.unbounded = b.ceilings == work.Ceilings{}

	b.seeded = true
	return nil
}

// addReceipt counts each stable ID once, whether first seen locally or in a
// peer's durable journal. Keeping the union monotonic prevents a stale/deleted
// row from refunding spend. Caller holds b.mu.
func (b *runBudget) addReceipt(receiptID string, call work.CallSpend) error {
	if _, exists := b.receipts[receiptID]; exists {
		return nil
	}
	if len(b.receipts) >= maxTrackedRunReceipts {
		return fmt.Errorf("model usage receipt tracking reached its %d-receipt safety bound", maxTrackedRunReceipts)
	}
	if b.receipts == nil {
		b.receipts = map[string]struct{}{}
	}
	if b.pending == nil {
		b.pending = map[string]work.CallSpend{}
	}
	b.receipts[receiptID] = struct{}{}
	b.pending[receiptID] = call
	b.spent = work.AddCall(b.spent, call)
	return nil
}

// refreshReceipts reads only bounded usage summaries, never prompt/response
// bodies. Every page skips the result cache. A partial read may add known spend,
// but cannot authorize a call; a retry deduplicates the same IDs.
func (c *RunCeilings) refreshReceipts(ctx context.Context, rc common.RunContext, b *runBudget) error {
	if b.receiptOverflow {
		return fmt.Errorf("unjournaled model usage exceeded the recoverable pending index")
	}
	// This is a bounded-time accounting read, not a lifetime receipt cap.
	// Stable (createdAt, ID) ordering proves uniqueness with one watermark across pages;
	// only not-yet-journaled local charges require an active duplicate index.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	query := "query " + call("workModelCallsPageForOwnerRun", map[string]any{"runId": rc.RunId})
	var total work.Spent
	pending := make(map[string]work.CallSpend, len(b.pending))
	for id, spend := range b.pending {
		pending[id] = spend
	}
	lastID, cursor := "", ""
	var lastAt time.Time
	emptyPages := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := c.store.engine.Execute(memqlengine.ContextWithCursor(ownerActor(memqlengine.ContextWithFreshRead(ctx), rc.OwnerUserId), cursor), query)
		if err != nil {
			return err
		}
		if result == nil {
			return fmt.Errorf("model usage returned no page")
		}
		rows := memqlRows(result)
		// Node-set materialization need not preserve the query's page order.
		// Restore it inside the page before checking the cross-page watermark.
		sort.Slice(rows, func(a, b int) bool {
			at, _ := rowTime(rows[a], "createdAt")
			bt, _ := rowTime(rows[b], "createdAt")
			if !at.Equal(bt) {
				return at.Before(bt)
			}
			return rowString(rows[a], "id") < rowString(rows[b], "id")
		})
		if len(rows) == 0 {
			emptyPages++
		} else {
			emptyPages = 0
		}
		if emptyPages > 32 {
			return fmt.Errorf("model usage pagination made no progress")
		}
		for _, row := range rows {
			receiptID := memqlengine.BareShortId(rowString(row, "id"))
			at, validTime := rowTime(row, "createdAt")
			if !validTime || receiptID == "" || (!lastAt.IsZero() && (at.Before(lastAt) || (at.Equal(lastAt) && receiptID <= lastID))) {
				return fmt.Errorf("model usage identities did not advance in stable order (%q after %q)", receiptID, lastID)
			}
			lastID = receiptID
			lastAt = at
			if memqlengine.BareShortId(rowString(row, "ownerUserId")) != memqlengine.BareShortId(strings.TrimSpace(rc.OwnerUserId)) || memqlengine.BareShortId(rowString(row, "runId")) != memqlengine.BareShortId(rc.RunId) {
				return fmt.Errorf("model usage receipt attribution does not match this run")
			}
			total = work.AddCall(total, work.CallSpend{Served: rowString(row, "served"), InputTokens: rowInt(row, "inputTokens"), OutputTokens: rowInt(row, "outputTokens"), Cost: rowFloat(row, "cost")})
			delete(pending, receiptID)
		}
		next := ""
		if meta := result.GetMeta(); meta != nil {
			next = meta.Cursor
			if meta.HasMore && next == "" {
				return fmt.Errorf("model usage has more rows without a cursor")
			}
		}
		if next == "" {
			break
		}
		if next == cursor {
			return fmt.Errorf("model usage cursor did not advance")
		}
		cursor = next
	}
	if total.ModelCalls < b.durable.ModelCalls || total.Tokens < b.durable.Tokens || total.TokensLocal < b.durable.TokensLocal || total.TokensSubscription < b.durable.TokensSubscription || total.Cost < b.durable.Cost {
		return fmt.Errorf("durable model usage regressed; accounting recovery is required before another call")
	}
	b.durable = total
	b.pending = pending
	b.receipts = map[string]struct{}{}
	b.spent = total
	for id, spend := range pending {
		b.receipts[id] = struct{}{}
		b.spent = work.AddCall(b.spent, spend)
	}
	b.spent.ModelCalls += b.unrecorded.ModelCalls
	b.spent.Tokens += b.unrecorded.Tokens
	b.spent.TokensLocal += b.unrecorded.TokensLocal
	b.spent.TokensSubscription += b.unrecorded.TokensSubscription
	b.spent.Cost += b.unrecorded.Cost
	if len(b.pending) >= maxTrackedRunReceipts {
		return fmt.Errorf("unjournaled model usage reached the bounded pending index; durable receipts must be recovered")
	}
	return nil
}

// goalCeilings reads one goal's declared ceilings under the run owner's
// borrowed authority, refusing rather than answering "none declared" for a
// goal it could not read.
func (c *RunCeilings) goalCeilings(ctx context.Context, rc common.RunContext) (work.Ceilings, error) {
	owner := strings.TrimSpace(rc.OwnerUserId)
	if owner == "" {
		return work.Ceilings{}, fmt.Errorf("run %s names goal %s and carries no owner, so its ceilings cannot be read as anybody", rc.RunId, rc.GoalId)
	}
	goal, err := c.store.goalForOwner(ownerActor(memqlengine.ContextWithFreshRead(ctx), owner), rc.GoalId)
	if err != nil {
		return work.Ceilings{}, fmt.Errorf("reading goal %s: %w", rc.GoalId, err)
	}
	if goal == nil {
		return work.Ceilings{}, fmt.Errorf("goal %s is not readable as %s", rc.GoalId, owner)
	}
	ceilings, err := ceilingsOf(goal)
	if rowString(goal, "status") == "closed" {
		return work.Ceilings{}, fmt.Errorf("the work goal is closed")
	}
	if err != nil {
		return work.Ceilings{}, fmt.Errorf("goal %s ceilings: %w", rc.GoalId, err)
	}
	run, err := c.store.runForOwner(ownerActor(memqlengine.ContextWithFreshRead(ctx), owner), rc.RunId)
	if err != nil || run == nil {
		return work.Ceilings{}, fmt.Errorf("run workload is unavailable: %v", err)
	}
	return progressCeilings(goal, run, ceilings)
}

// enforceableHere drops the ceilings this seam cannot evaluate.
//
// maxRetries and maxEvents are the EXECUTOR's counters -- the run's
// re-attempts and its published events -- and neither reaches a model call.
// Handing CheckCeilings a zero for them would clear them SILENTLY on every
// call, which is the shape of the defect this whole file exists to close. The
// failure path enforces maxRetries; maxEvents is warned about once per run.
func enforceableHere(c work.Ceilings) work.Ceilings {
	c.MaxRetries, c.MaxEvents = 0, 0
	return c
}

// warnAboutUnenforceable says once, per run, which declared ceilings nothing
// enforces. Silence here would let an operator believe a number they set is a
// limit. maxRetries is not among them: the failure path counts the run's
// re-attempts against it (memql#5664), so warning about it would tell the
// operator the opposite of what is true.
func (c *RunCeilings) warnAboutUnenforceable(rc common.RunContext, ceilings work.Ceilings) {
	var unenforced []string
	if ceilings.MaxEvents > 0 {
		unenforced = append(unenforced, work.CeilingEvents)
	}
	if len(unenforced) == 0 || c.logger == nil {
		return
	}
	c.logger.Warn("work: this goal declares a ceiling nothing enforces, so it bounds nothing",
		"component", "work.ceilings", "run", rc.RunId, "goal", rc.GoalId,
		"unenforced", strings.Join(unenforced, ","),
		"why", "a run's published events are the executor's counter, reach no model call, and no executor counts them yet")
}

// The owner and goal are part of the cache identity. A different actor cannot
// borrow a seeded result by presenting the same run ID.
func runBudgetKey(rc common.RunContext) string {
	return memqlengine.BareShortId(strings.TrimSpace(rc.OwnerUserId)) + "\x00" + memqlengine.BareShortId(rc.GoalId) + "\x00" + memqlengine.BareShortId(rc.RunId)
}

// budgetFor returns the run's counters, creating them on first sight.
func (c *RunCeilings) budgetFor(rc common.RunContext) *runBudget {
	key := runBudgetKey(rc)
	now := c.clock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if b, ok := c.runs[key]; ok {
		b.touched = now
		return b
	}
	c.evictIfFullLocked(now)
	b := &runBudget{touched: now}
	c.runs[key] = b
	return b
}

// evictIfFullLocked drops the least recently touched run when the table is
// full. Called with c.mu held.
func (c *RunCeilings) evictIfFullLocked(now time.Time) {
	if len(c.runs) < maxTrackedRuns {
		return
	}
	oldestKey, oldest := "", now
	for k, b := range c.runs {
		if oldestKey == "" || b.touched.Before(oldest) {
			oldestKey, oldest = k, b.touched
		}
	}
	if oldestKey != "" {
		delete(c.runs, oldestKey)
	}
}

func (c *RunCeilings) clock() time.Time {
	if c == nil || c.now == nil {
		return time.Now()
	}
	return c.now()
}

// breachOf converts the decision into the shape the engine speaks. A nil
// breach stays nil -- an explicit return, because a typed nil inside an
// interface is not nil.
func breachOf(b *work.CeilingBreach) *memqlengine.RunCeilingBreach {
	if b == nil {
		return nil
	}
	return &memqlengine.RunCeilingBreach{
		Ceiling: b.Ceiling,
		Limit:   b.Limit,
		Actual:  b.Actual,
		Reason:  b.Reason,
	}
}

// Deadline lets an admitted operation finish within the original hard limit.
// Soft workload windows are checked between calls, where progress can be saved
// and assessed. Neither a retry nor a replica hop restarts the clock.
func (c *RunCeilings) Deadline(ctx context.Context, rc common.RunContext) (time.Time, error) {
	if rc.GoalId == "" {
		return time.Time{}, nil
	}
	ctx = memqlengine.ContextWithFreshRead(ctx)
	b := c.budgetFor(rc)
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := c.seed(ctx, rc, b); err != nil {
		return time.Time{}, err
	}
	if err := c.refreshReceipts(ctx, rc, b); err != nil {
		return time.Time{}, err
	}
	declared, err := c.goalCeilings(ctx, rc)
	if err != nil {
		return time.Time{}, err
	}
	b.ceilings = enforceableHere(declared)
	b.unbounded = b.ceilings == work.Ceilings{}
	// Re-entry may land on this same replica; a cached budget must see the
	// committed wait credit without resetting any model or token counters.
	run, err := c.store.runForOwner(ownerActor(ctx, rc.OwnerUserId), rc.RunId)
	if err != nil || run == nil {
		return time.Time{}, fmt.Errorf("run wait budget is unavailable: %v", err)
	}
	b.humanWaitMs = int64(rowInt(run, "humanWaitMs"))
	return c.DeclaredDeadline(ctx, rc)
}

// DeclaredDeadline verifies the goal/run binding but imposes no lifetime
// deadline. A context deadline cannot be renewed by progress after it is made,
// and used to kill productive tool loops at two hours. Admission checks renewable
// windows; individual model/tool requests retain their own timeout and idle
// watchdogs. Cancellation and permission expiry remain independent controls.
func (c *RunCeilings) DeclaredDeadline(ctx context.Context, rc common.RunContext) (time.Time, error) {
	if rc.GoalId == "" {
		return time.Time{}, nil
	}
	ctx = ownerActor(memqlengine.ContextWithFreshRead(ctx), rc.OwnerUserId)
	run, err := c.store.runForOwner(ctx, rc.RunId)
	if err != nil || run == nil {
		return time.Time{}, fmt.Errorf("run deadline unavailable: %v", err)
	}
	if memqlengine.BareShortId(rowString(run, "goalId")) != memqlengine.BareShortId(rc.GoalId) {
		return time.Time{}, fmt.Errorf("run does not belong to the named goal")
	}
	goal, err := c.store.goalForOwner(ctx, rc.GoalId)
	if err != nil || goal == nil {
		return time.Time{}, fmt.Errorf("goal deadline unavailable: %v", err)
	}
	if _, err := ceilingsOf(goal); err != nil {
		return time.Time{}, err
	}
	return time.Time{}, nil
}
