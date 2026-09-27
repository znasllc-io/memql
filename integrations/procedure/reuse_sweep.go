package procedure

// reuse_sweep.go -- every construct's reuse label, decided by the evidence
// (epic memql#5414, #5418; design D24).
//
// The decomposer PROPOSES a reuse intent, the EVIDENCE decides the label, and
// a person may override it (reuse.go). The evidence for one construct is the
// set of the owner's succeeded runs that USED it, in any of the three ways a
// construct is used:
//
//	as the run's compiled template     run.templateConstructId names it
//	as the procedure a replay served   a replayLearnedProcedure run whose
//	                                   variables.procedureConstructId names it
//	as a section of another goal       an automation step of the run calls it
//	                                   by name (a decomposed goal's catalog
//	                                   section is exactly that)
//
// and the label is component/work.DecideReuse over the distinct goal
// signatures, the account ties of those runs' goals and the use count, at the
// feedbackPolicy's threshold. A procedure's own replay run (triggeredBy
// `procedure:`) is not a use: the goal run that named replayLearnedProcedure
// already is, and the shadow comparisons a replay run also records served no
// goal at all.
//
// THE READS SPAN OWNERS BY NATURE, and a construct has NO cluster-owner arm:
// read as the maintenance principal every one answers zero rows and no error,
// and a catalog in which nothing is labelled looks exactly like one in which
// nothing has been reused yet. So the sweep lists owners through the one read
// made as the cluster (usersForSeedSweep, beside the stamp in store.go) and
// reads each owner's runs, steps, goals and constructs under THAT owner's own
// actor -- evidence counted across two people's work would call a construct
// reusable that neither of them has reused. Hence the gate, which is the
// ladder sweep's (sweep.go): only the maintenance principal, or trusted
// server-side Go under internal origin, may run it.
//
// ONLY A CHANGED CONSTRUCT IS WRITTEN, and the person's override is never
// touched: recordConstructReuse takes no reuseOverride argument at all. The
// stamp of when the evidence was decided is not a change on its own, or every
// construct would be rewritten every six hours with no information in it.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// replayAutomationName is the one embedded template a learned procedure
// serves a goal through (dsl/procedure/automations.memql).
const replayAutomationName = "replayLearnedProcedure"

// maxKeptSignatures bounds the signatures a construct's evidence keeps; the
// count beside them is the whole number.
const maxKeptSignatures = 20

// ReuseSweepResult is what one reuse sweep did.
type ReuseSweepResult struct {
	Owners int
	// Constructs is every construct the sweep labelled.
	Constructs int
	// Changed are the constructs whose label or evidence changed: written,
	// or -- on a dry run -- that a real sweep would write.
	Changed []string
	// Errors counts owners or constructs the sweep could not read or write;
	// one failure never stops the rest.
	Errors int
	DryRun bool
}

// ReuseSweep decides every owner's construct labels.
func (i *Integration) ReuseSweep(ctx context.Context, dryRun bool) (ReuseSweepResult, error) {
	res := ReuseSweepResult{DryRun: dryRun}
	ac, _ := auth.AccessFromContext(ctx)
	if !isClusterPrincipal(ac) && !auth.OriginFromContext(ctx).IsInternal() {
		return res, fmt.Errorf("procedure.reuseSweep: the reuse sweep reads every owner's work and runs as the cluster's " +
			"maintenance principal; a person labels one of their constructs with setConstructReuse")
	}
	owners, err := i.ownerIds(ctx)
	if err != nil {
		return res, fmt.Errorf("procedure.reuseSweep: listing owners: %w", err)
	}
	res.Owners = len(owners)
	for _, owner := range owners {
		i.reuseSweepOwner(ctx, owner, &res)
	}
	i.log().Info("procedure: reuse sweep finished", "owners", res.Owners, "constructs", res.Constructs,
		"changed", len(res.Changed), "errors", res.Errors, "dryRun", dryRun)
	return res, nil
}

func (i *Integration) handleReuseSweep(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	dryRun, _ := args["dryRun"].(bool)
	res, err := i.ReuseSweep(ctx, dryRun)
	if err != nil {
		return nil, err
	}
	return i.reply(map[string]any{
		"owners":     res.Owners,
		"constructs": res.Constructs,
		"changed":    append([]string{}, res.Changed...),
		"errors":     res.Errors,
		"dryRun":     res.DryRun,
	}), nil
}

// reuseSweepOwner labels one owner's constructs from that owner's own work.
func (i *Integration) reuseSweepOwner(ctx context.Context, owner string, res *ReuseSweepResult) {
	actorCtx := ownerActor(ctx, owner)
	constructs, err := i.reusableConstructs(actorCtx)
	if err != nil {
		i.log().Warn("procedure: the reuse sweep could not read an owner's constructs", "owner", owner, "error", err)
		res.Errors++
		return
	}
	if len(constructs) == 0 {
		return
	}
	uses, err := i.readUses(actorCtx)
	if err != nil {
		i.log().Warn("procedure: the reuse sweep could not read an owner's work", "owner", owner, "error", err)
		res.Errors++
		return
	}
	policy := i.feedbackPolicy(actorCtx)
	now := i.clock().UTC().Format(timeLayout)
	for _, c := range constructs {
		res.Constructs++
		ev := uses.evidenceFor(c)
		label := work.DecideReuse(ev, policy.ReusableAfterSignatures)
		if !reuseChanged(c, label, ev) {
			continue
		}
		constructId := str(c, "id")
		res.Changed = append(res.Changed, constructId)
		if res.DryRun {
			continue
		}
		args := map[string]any{"constructId": constructId, "reuse": string(label), "reuseEvidence": evidenceObject(ev, now)}
		if err := i.store.writeInternal(actorCtx, "mutation "+call("recordConstructReuse", args)); err != nil {
			i.log().Warn("procedure: the reuse sweep could not write a construct's label", "constructId", constructId, "error", err)
			res.Changed = res.Changed[:len(res.Changed)-1]
			res.Errors++
			continue
		}
		i.log().Debug("procedure: the reuse sweep relabelled a construct", "constructId", constructId,
			"reuse", string(label), "signatures", len(ev.GoalSignatures), "uses", ev.Uses)
	}
}

// reusableConstructs is every automation of the owner's that can be reused:
// the catalogued authored ones and the learned procedures, each once. A
// construct of another kind is never invoked as a whole and has no reuse to
// decide, and a withdrawn one is not labelled.
func (i *Integration) reusableConstructs(ctx context.Context) ([]map[string]any, error) {
	seen := map[string]bool{}
	var out []map[string]any
	for _, q := range []string{"cataloguedConstructsForOwner", "learnedProceduresForOwner"} {
		rows, err := i.readAllPages(ctx, "query "+call(q, nil))
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			id := memqlengine.BareShortId(str(r, "id"))
			if id == "" || seen[id] || str(r, "kind") != "automation" || str(r, "status") == "retired" {
				continue
			}
			seen[id] = true
			out = append(out, r)
		}
	}
	return out, nil
}

// constructUses is one owner's work, indexed the three ways a construct is
// used.
type constructUses struct {
	// runs are the owner's signed, succeeded runs by bare id.
	runs map[string]map[string]any
	// byTemplate and byProcedure index those runs by the construct they name
	// (bare id); byName by the automation their steps call.
	byTemplate, byProcedure, byName map[string][]string
	// accounts are each goal's account ties, by bare goal id.
	accounts map[string][]string
}

// readUses reads the owner's signed runs, automation steps and goals.
func (i *Integration) readUses(ctx context.Context) (*constructUses, error) {
	u := &constructUses{
		runs:        map[string]map[string]any{},
		byTemplate:  map[string][]string{},
		byProcedure: map[string][]string{},
		byName:      map[string][]string{},
		accounts:    map[string][]string{},
	}
	runs, err := i.readAllPages(ctx, "query "+call("workSignedRunsForOwner", nil))
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		id := memqlengine.BareShortId(str(r, "id"))
		// The query filters on both; checked again, because a run that is
		// not what the filter promised would be counted as evidence that a
		// construct served a goal.
		if id == "" || str(r, "status") != "succeeded" || strings.TrimSpace(str(r, "goalSignature")) == "" {
			continue
		}
		u.runs[id] = r
		if t := memqlengine.BareShortId(str(r, "templateConstructId")); t != "" && !isReplayRun(r) {
			u.byTemplate[t] = append(u.byTemplate[t], id)
		}
		if str(r, "automationName") == replayAutomationName {
			if p := memqlengine.BareShortId(str(obj(r, "variables"), "procedureConstructId")); p != "" {
				u.byProcedure[p] = append(u.byProcedure[p], id)
			}
		}
	}
	steps, err := i.readAllPages(ctx, "query "+call("workAutomationStepsForOwner", nil))
	if err != nil {
		return nil, err
	}
	for _, s := range steps {
		name := strings.TrimSpace(str(obj(s, "call"), "name"))
		runId := memqlengine.BareShortId(str(s, "runId"))
		if str(s, "stepType") != "automation" || name == "" || u.runs[runId] == nil {
			continue
		}
		// A step that did not finish did not use the construct.
		if status := str(s, "status"); status != "" && status != "done" {
			continue
		}
		u.byName[name] = append(u.byName[name], runId)
	}
	goals, err := i.readAllPages(ctx, "query "+call("workGoalsForOwner", nil))
	if err != nil {
		return nil, err
	}
	for _, g := range goals {
		if id := memqlengine.BareShortId(str(g, "id")); id != "" {
			u.accounts[id] = stringList(g["accountIds"])
		}
	}
	return u, nil
}

// evidenceFor is what the owner's work says about one construct: the distinct
// signatures and account ties of the runs that used it, and how many runs did.
func (u *constructUses) evidenceFor(c map[string]any) work.ReuseEvidence {
	id := memqlengine.BareShortId(str(c, "id"))
	used := map[string]bool{}
	for _, runId := range u.byTemplate[id] {
		used[runId] = true
	}
	for _, runId := range u.byProcedure[id] {
		used[runId] = true
	}
	for _, runId := range u.byName[strings.TrimSpace(str(c, "name"))] {
		used[runId] = true
	}
	sigs, accounts := map[string]bool{}, map[string]bool{}
	for runId := range used {
		run := u.runs[runId]
		sigs[strings.TrimSpace(str(run, "goalSignature"))] = true
		for _, a := range u.accounts[memqlengine.BareShortId(str(run, "goalId"))] {
			if a = strings.TrimSpace(a); a != "" {
				accounts[a] = true
			}
		}
	}
	return work.ReuseEvidence{GoalSignatures: sortedKeys(sigs), AccountIds: sortedKeys(accounts), Uses: len(used)}
}

// evidenceObject is the stored form of the evidence: the signatures sorted
// and at most maxKeptSignatures of them, with the whole count beside them.
// The lists are never nil, so an empty one is stored as [] rather than
// dropped from the row.
func evidenceObject(ev work.ReuseEvidence, decidedAt string) map[string]any {
	kept := append([]string{}, ev.GoalSignatures...)
	if len(kept) > maxKeptSignatures {
		kept = kept[:maxKeptSignatures]
	}
	return map[string]any{
		"goalSignatures": kept,
		"signatureCount": len(ev.GoalSignatures),
		"accountIds":     append([]string{}, ev.AccountIds...),
		"uses":           ev.Uses,
		"decidedAt":      decidedAt,
	}
}

// reuseChanged reports whether a construct's stored label or evidence differs
// from what the sweep decided. decidedAt is not compared: when a sweep looked
// is not news.
func reuseChanged(c map[string]any, label work.ReuseLabel, ev work.ReuseEvidence) bool {
	if work.ParseReuseLabel(str(c, "reuse")) != label {
		return true
	}
	stored := obj(c, "reuseEvidence")
	if stored == nil {
		return true
	}
	want := evidenceObject(ev, "")
	return !sameStrings(stringList(stored["goalSignatures"]), want["goalSignatures"].([]string)) ||
		!sameStrings(stringList(stored["accountIds"]), want["accountIds"].([]string)) ||
		intOf(stored, "signatureCount") != want["signatureCount"].(int) ||
		intOf(stored, "uses") != want["uses"].(int)
}

// feedbackPolicy reads the epic's values, as the owner. A row that is absent
// or unreadable is component/work.DefaultFeedbackPolicy(), which carries the
// seed's values.
func (i *Integration) feedbackPolicy(ctx context.Context) work.FeedbackPolicy {
	rows, err := i.store.query(ctx, "query "+call("feedbackPolicyCurrent", nil))
	if err != nil || len(rows) == 0 {
		if err != nil {
			i.log().Debug("procedure: the feedback policy row is not readable; applying its seeded values", "error", err)
		}
		return work.FeedbackPolicyFrom(nil)
	}
	return work.FeedbackPolicyFrom(rows[0])
}

// readAllPages reads every page of a paginated read under the actor in ctx,
// bounded like the ladder sweep's walk: a cursor that never ends is a bug to
// stop at rather than a loop to run forever.
func (i *Integration) readAllPages(ctx context.Context, q string) ([]map[string]any, error) {
	var out []map[string]any
	cursor := ""
	for page := 0; page < maxSweepPages; page++ {
		rows, next, err := i.store.queryPage(ctx, cursor, q)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if next == "" || next == cursor || len(rows) == 0 {
			return out, nil
		}
		cursor = next
	}
	i.log().Warn("procedure: a read stopped paging at its bound", "query", firstConstruct(q), "pages", maxSweepPages)
	return out, nil
}

func sameStrings(a, b []string) bool {
	a, b = append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	if len(a) != len(b) {
		return false
	}
	for n := range a {
		if a[n] != b[n] {
			return false
		}
	}
	return true
}
