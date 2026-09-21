package work

// runceilings.go -- the run's ceilings, read and enforced (memql#5580).
//
// component/work.CheckCeilings was written, documented and unit-tested with no
// production caller: a goal's costCeiling, tokenBudget and maxModelCalls were
// settable fields nothing read. This is the caller. component/memql holds the
// SEAM (run_ceilings.go, which says why it is that seam and not the step
// runner, the journal or the router); this holds the decision, beside the rows
// it is made from -- exactly as ModelJournal holds work.DecideServe's.
//
// WHAT IT READS, AND HOW OFTEN. Once per run per process:
//
//	the run row     for startedAt (the wall-clock ceiling) and to confirm the
//	                goal the context names.
//	the goal row    for ceilings.
//	its modelCall   folded into the run's spend so far -- which is what makes
//	                rows        the count survive a resume onto another
//	                replica. See "RE-ENTRY" below.
//
// Thereafter every admit is an in-memory counter read and every charge an
// in-memory add -- with ONE exception: an admit that is about to REFUSE
// re-reads the goal's ceilings first, so a person who raised the ceiling the
// budget approval asked them to raise is not answered with the old number. It
// re-reads the ceilings only, never the spend.
//
// THE BLAST RADIUS IS GOAL-BACKED RUNS, DELIBERATELY. A run with no goalId on
// its context inherits no ceilings -- ceilings live on v1:work:goal, and an
// automation's run has no goal -- so it is admitted with no read at all. That
// is not a silent pass: there is nothing declared to evaluate. Every automation
// run in the cluster therefore costs this guard nothing, and the runs that DID
// declare a ceiling are the only ones that can be refused by one.
//
// WHERE IT FAILS CLOSED. A run whose goal is named and whose ceilings cannot
// be READ is refused, with `unevaluated` as the ceiling name: a limit nobody
// could read and a limit nobody set are the same number, and only one of them
// means unbounded on purpose. The seed is retried on the next call, so a
// transient database blip costs one refused call rather than the run.
//
// WHAT IT CANNOT EVALUATE, AND SAYS SO. maxRetries and maxEvents are the
// EXECUTOR's counters -- a step's attempts and a run's published events -- and
// neither reaches a model call. Passing a zero for them to CheckCeilings would
// clear them silently on every call, so they are cleared from the ceilings
// this seam checks and WARNED about once per run instead. An operator who set
// one is told it is not enforced here rather than believing it is.
//
// RE-ENTRY: SPEND ACCUMULATES, A NEW RUN STARTS FRESH. A resumed or
// re-dispatched run is the same v1:work:run id, so the seed folds the calls it
// already made and the second attempt continues the first one's budget -- park,
// approve, park again is not a way to buy another allowance. A replay or a
// fork is a DIFFERENT run id with its own (empty) journal and its own
// ceilings, which is right: it is a different run. The one gap is a modelCall
// row whose write failed (the journal is best-effort by design), which
// under-counts by that call across a re-entry and not within a process.

import (
	"context"
	"fmt"
	"log/slog"
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
	seeded bool
	// ceilings are the goal's, with the two this seam cannot evaluate
	// cleared. unbounded is true when nothing is left to check.
	ceilings  work.Ceilings
	unbounded bool
	spent     work.Spent
	startedAt time.Time
	touched   time.Time
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
	if b.unbounded {
		return nil
	}
	spent := b.spent
	if !b.startedAt.IsZero() {
		spent.WallClockMs = c.clock().Sub(b.startedAt).Milliseconds()
	}
	breach := work.CheckCeilings(b.ceilings, spent, estimatedTokens)
	if breach == nil {
		return nil
	}

	// A REFUSAL RE-READS THE GOAL BEFORE IT STANDS, because the whole point of
	// parking rather than failing is that a person can raise the ceiling and
	// carry on -- and the approval says so in those words. The ceilings are
	// otherwise read once per run per process, so a run re-dispatched onto
	// THIS process after its ceiling was raised would meet the old number and
	// park again, with the approval's promise plainly untrue.
	//
	// It re-reads only the CEILINGS, never the spend: re-folding the journal
	// would discard the in-process counts that no row records -- a cache hit
	// is not journaled -- and quietly hand a looping run its allowance back.
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
	b.spent = work.AddCall(b.spent, work.CallSpend{
		Served:       spend.Served,
		InputTokens:  spend.InputTokens,
		OutputTokens: spend.OutputTokens,
		Cost:         spend.Cost,
	})
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
		out.WallClockMs = c.clock().Sub(b.startedAt).Milliseconds()
	}
	return out
}

// seed reads the run, its goal and its journal once. A seeded budget returns
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
	scoped := ownerActor(ctx, owner)

	run, err := c.store.runForOwner(scoped, rc.RunId)
	if err != nil {
		return fmt.Errorf("reading run %s: %w", rc.RunId, err)
	}
	if run == nil {
		return fmt.Errorf("run %s is not readable as %s", rc.RunId, owner)
	}
	b.startedAt, _ = time.Parse(time.RFC3339Nano, rowString(run, "startedAt"))

	declared, err := c.goalCeilings(ctx, rc)
	if err != nil {
		return err
	}
	// Warned on the DECLARED set, before the two this seam cannot see are
	// cleared: after the clearing there is nothing left to warn about.
	c.warnAboutUnenforceable(rc, declared)
	b.ceilings = enforceableHere(declared)
	b.unbounded = b.ceilings == work.Ceilings{}

	if !b.unbounded {
		rows, err := readModelCalls(ctx, c.store, owner, rc.RunId)
		if err != nil {
			return fmt.Errorf("reading run %s's model calls: %w", rc.RunId, err)
		}
		// FOLDED INTO A LOCAL AND ADDED, never assigned. Two reasons, and
		// both are about staying wrong in the safe direction:
		//
		//   - A partial fold must not survive a failed read. Assigning as we
		//     go leaves half a journal behind when the read errors, and the
		//     retry then folds the same rows on top of it.
		//   - An in-process charge must not be discarded. Nothing charges
		//     before the first admit today, so there is nothing here to add
		//     to -- but if that ever stops being true, ADDING over-counts
		//     (the run stops early, visibly) where assigning would silently
		//     hand it its allowance back.
		journaled := work.Spent{}
		for _, row := range rows {
			journaled = work.AddCall(journaled, work.CallSpend{
				Served:       rowString(row, "served"),
				InputTokens:  rowInt(row, "inputTokens"),
				OutputTokens: rowInt(row, "outputTokens"),
				Cost:         rowFloat(row, "cost"),
			})
		}
		b.spent = addSpent(b.spent, journaled)
	}
	b.seeded = true
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
	goal, err := c.store.goalForOwner(ownerActor(ctx, owner), rc.GoalId)
	if err != nil {
		return work.Ceilings{}, fmt.Errorf("reading goal %s: %w", rc.GoalId, err)
	}
	if goal == nil {
		return work.Ceilings{}, fmt.Errorf("goal %s is not readable as %s", rc.GoalId, owner)
	}
	ceilings, err := ceilingsOf(goal)
	if err != nil {
		return work.Ceilings{}, fmt.Errorf("goal %s ceilings: %w", rc.GoalId, err)
	}
	return ceilings, nil
}

// addSpent sums two spends, bucket by bucket.
func addSpent(a, b work.Spent) work.Spent {
	return work.Spent{
		Tokens:             a.Tokens + b.Tokens,
		TokensSubscription: a.TokensSubscription + b.TokensSubscription,
		TokensLocal:        a.TokensLocal + b.TokensLocal,
		Cost:               a.Cost + b.Cost,
		ModelCalls:         a.ModelCalls + b.ModelCalls,
		Retries:            a.Retries + b.Retries,
		Events:             a.Events + b.Events,
		WallClockMs:        a.WallClockMs + b.WallClockMs,
	}
}

// enforceableHere drops the ceilings this seam cannot evaluate.
//
// maxRetries and maxEvents are the EXECUTOR's counters -- a step's attempts
// and a run's published events -- and neither reaches a model call. Handing
// CheckCeilings a zero for them would clear them SILENTLY on every call, which
// is the shape of the defect this whole file exists to close. Cleared here and
// warned about once per run instead.
func enforceableHere(c work.Ceilings) work.Ceilings {
	c.MaxRetries, c.MaxEvents = 0, 0
	return c
}

// warnAboutUnenforceable says once, per run, which declared ceilings this seam
// does not evaluate. Silence here would let an operator believe a number they
// set is a limit.
func (c *RunCeilings) warnAboutUnenforceable(rc common.RunContext, ceilings work.Ceilings) {
	var unenforced []string
	if ceilings.MaxRetries > 0 {
		unenforced = append(unenforced, work.CeilingRetries)
	}
	if ceilings.MaxEvents > 0 {
		unenforced = append(unenforced, work.CeilingEvents)
	}
	if len(unenforced) == 0 || c.logger == nil {
		return
	}
	c.logger.Warn("work: this goal declares ceilings the model seam cannot evaluate, so they bound nothing here",
		"component", "work.ceilings", "run", rc.RunId, "goal", rc.GoalId,
		"unenforced", strings.Join(unenforced, ","),
		"why", "a step's attempts and a run's published events are the executor's counters and reach no model call")
}

// budgetFor returns the run's counters, creating them on first sight.
func (c *RunCeilings) budgetFor(rc common.RunContext) *runBudget {
	key := memqlengine.BareShortId(rc.RunId)
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
