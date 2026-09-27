package procedure

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// sweep.go -- the certification ladder's two maintenance sweeps (epic
// memql#5408, D14's retirement, D16; plan Task 5 step 7).
//
//	demotion    every fifteen minutes: every canary and trusted procedure's
//	            STORED evidence re-evaluated under the CURRENT policy, so a
//	            threshold that tightened demotes a procedure without waiting
//	            for its next replay
//	retirement  nightly: every procedure unused for longer than the policy's
//	            window retires, on any rung
//
// Both are component/work.Advance(EventSweep); the split is which half of it
// each writes. The demotion sweep tells Advance nothing about use, so it can
// only demote -- "an unknown last use is never evidence of disuse" -- and the
// retirement sweep writes only a transition that retired.
//
// THE READS SPAN OWNERS BY NATURE, and a construct has NO cluster-owner arm:
// read as the maintenance principal it answers zero rows and no error, and a
// ladder that never moves looks exactly like one with nothing to move. So the
// sweep lists the owners through the one read made as the cluster
// (usersForSeedSweep, the stamped read store.go keeps), and reads and writes
// each owner's procedures under THAT owner's own actor. Hence the gate: only
// the maintenance principal, or trusted server-side Go under internal origin,
// may run it.
//
// Only a row that CHANGED is written. The sweep runs every fifteen minutes
// over every procedure in the cluster, and a write per row per pass is growth
// with no information in it.

// SweepDemotion and SweepRetirement are the two sweeps.
const (
	SweepDemotion   = "demotion"
	SweepRetirement = "retirement"
)

// maxSweepPages bounds one owner's walk: 100 procedures a page is far past
// what anybody accumulates, and a cursor that never ends is a bug to stop at
// rather than a loop to run forever.
const maxSweepPages = 1000

// SweepResult is what one sweep did.
type SweepResult struct {
	Sweep  string
	Owners int
	// Procedures is every learned procedure read; Examined those on a rung.
	Procedures int
	Examined   int
	Demoted    int
	Retired    int
	// Changed are the constructs written.
	Changed []string
	// Errors counts owners or rows the sweep could not read or write; one
	// failure never stops the rest.
	Errors int
}

// Sweep runs one of the ladder's two sweeps at now.
func (i *Integration) Sweep(ctx context.Context, sweep string, now time.Time) (SweepResult, error) {
	sweep = strings.TrimSpace(sweep)
	res := SweepResult{Sweep: sweep}
	if sweep != SweepDemotion && sweep != SweepRetirement {
		return res, fmt.Errorf("procedure.ladderSweep: sweep %q is not demotion or retirement", sweep)
	}
	ac, _ := auth.AccessFromContext(ctx)
	if !isClusterPrincipal(ac) && !auth.OriginFromContext(ctx).IsInternal() {
		return res, fmt.Errorf("procedure.ladderSweep: the ladder's sweeps span every owner and run as the cluster's maintenance principal; " +
			"a person's procedures move on their own replays")
	}
	owners, err := i.ownerIds(ctx)
	if err != nil {
		return res, fmt.Errorf("procedure.ladderSweep: listing owners: %w", err)
	}
	res.Owners = len(owners)
	for _, owner := range owners {
		i.sweepOwner(ctx, sweep, owner, now.UTC(), &res)
	}
	i.log().Info("procedure: ladder sweep finished",
		"sweep", sweep, "owners", res.Owners, "procedures", res.Procedures,
		"demoted", res.Demoted, "retired", res.Retired, "errors", res.Errors)
	return res, nil
}

// sweepOwner walks one owner's learned procedures, every page, as the owner.
func (i *Integration) sweepOwner(ctx context.Context, sweep, owner string, now time.Time, res *SweepResult) {
	actorCtx := ownerActor(ctx, owner)
	policy := i.readPolicy(ctx, owner)
	cursor := ""
	for page := 0; page < maxSweepPages; page++ {
		rows, next, err := i.store.queryPage(actorCtx, cursor, "query "+call("learnedProceduresForOwner", nil))
		if err != nil {
			i.log().Warn("procedure: the ladder sweep could not read an owner's procedures", "owner", owner, "error", err)
			res.Errors++
			return
		}
		for _, row := range rows {
			res.Procedures++
			i.sweepOne(ctx, sweep, owner, row, policy, now, res)
		}
		if next == "" || next == cursor || len(rows) == 0 {
			return
		}
		cursor = next
	}
	i.log().Warn("procedure: the ladder sweep stopped paging an owner's procedures at its bound", "owner", owner, "pages", maxSweepPages)
}

// sweepOne re-evaluates one procedure and writes it only when it moved.
func (i *Integration) sweepOne(ctx context.Context, sweep, owner string, row map[string]any, policy work.LadderPolicy, now time.Time, res *SweepResult) {
	state := ladderStateOf(row)
	if state.Rung == work.RungNone || state.Rung == work.RungRetired {
		// No rung is not a learned procedure's ladder; retired is terminal.
		return
	}
	res.Examined++
	ev := work.LadderEvent{Kind: work.EventSweep, At: now}
	if sweep == SweepRetirement {
		ev.LastUsedAt = lastUsed(state, row)
	}
	t := work.Advance(state, ev, policy)
	switch {
	case sweep == SweepDemotion && !t.Demoted:
		return
	case sweep == SweepRetirement && !t.Retired:
		return
	case !ladderMoved(state, t):
		return
	}
	constructId := str(row, "id")
	if err := i.writeLadder(ctx, owner, constructId, t); err != nil {
		i.log().Warn("procedure: the ladder sweep could not write a procedure", "constructId", constructId, "error", err)
		res.Errors++
		return
	}
	res.Changed = append(res.Changed, constructId)
	if t.Demoted {
		res.Demoted++
	}
	if t.Retired {
		res.Retired++
	}
	i.log().Info("procedure: the ladder sweep moved a procedure",
		"sweep", sweep, "constructId", constructId, "from", string(t.From), "to", string(t.To), "reason", t.Reason)
}

// lastUsed is when a procedure was last used: its last replay, or -- for one
// never replayed -- when its row was written. Never the zero time for a row
// that has either, and the zero time (never evidence of disuse) for one that
// has neither.
func lastUsed(state work.LadderState, row map[string]any) time.Time {
	created := timeOf(row, "createdAt")
	if state.LastReplayAt.After(created) {
		return state.LastReplayAt
	}
	return created
}

func (i *Integration) handleLadderSweep(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	res, err := i.Sweep(ctx, argString(args, "sweep"), i.clock())
	if err != nil {
		return nil, err
	}
	return i.reply(map[string]any{
		"sweep":      res.Sweep,
		"owners":     res.Owners,
		"procedures": res.Procedures,
		"examined":   res.Examined,
		"demoted":    res.Demoted,
		"retired":    res.Retired,
		"changed":    append([]string{}, res.Changed...),
		"errors":     res.Errors,
	}), nil
}

// queryPage runs one page of a paginated read under the actor in ctx, from
// cursor, and answers the rows and the engine's next cursor -- empty when the
// read is exhausted. The cursor rides the CONTEXT, which is the engine's
// contract: `paginate` declares a page size and the continuation is a runtime
// value, never a caller-spelled scan position in the query text.
func (s *store) queryPage(ctx context.Context, cursor, q string) ([]map[string]any, string, error) {
	if s == nil || s.engine == nil {
		return nil, "", fmt.Errorf("procedure: engine not configured")
	}
	if cursor != "" {
		ctx = memql.ContextWithCursor(ctx, cursor)
	}
	res, err := s.engine.Execute(ctx, q)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", firstConstruct(q), err)
	}
	next := ""
	if meta := res.GetMeta(); meta != nil {
		next = strings.TrimSpace(meta.Cursor)
	}
	return memqlRows(res), next, nil
}
