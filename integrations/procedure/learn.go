package procedure

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/num"
)

// learn.go -- the pipeline, in the record's order, and the two capability
// handlers over it.

// LiftOutcome says what a lift did to the owner's catalog.
type LiftOutcome string

const (
	// LiftNone: nothing cleared the floor, so nothing was lifted.
	LiftNone LiftOutcome = ""
	// LiftCreated: a new construct, entering the certification ladder.
	LiftCreated LiftOutcome = "created"
	// LiftRelifted: the owner's construct of that name learned a DIFFERENT
	// procedure -- a new version, written in place and put back on the
	// ladder's entry rung, because a person's approval was for the old one
	// (D15: "a later change of the construct is a new candidate").
	LiftRelifted LiftOutcome = "relifted"
	// LiftUnchanged: the same version as the one stored. Nothing is written
	// and every ladder counter is kept -- which is what lets a shadow streak
	// grow while the recordings it is measured on keep arriving.
	LiftUnchanged LiftOutcome = "unchanged"
)

// LearnResult is what one mining pass produced.
type LearnResult struct {
	// RunId is the run the pass was asked about; empty for a corpus sweep.
	RunId         string
	OwnerUserId   string
	GoalSignature string
	Level         Level
	Sequences     int
	Candidates    int
	// ConstructId is the lifted construct, empty when nothing cleared the
	// floor. An empty id with no error is the ORDINARY answer -- D14's floor
	// is two uses, and a corpus of one session is expected to yield nothing.
	ConstructId string
	Accepted    bool
	// Reason names why nothing was accepted, when nothing was. A sweep that
	// learned nothing and a sweep that could not read anything must not look
	// the same in a log.
	Reason string
	// Lift is what the lift did; ProcedureHash is the version it computed and
	// Rung the construct's rung after it -- written, or kept when unchanged.
	// A shadow comparison of the recording that triggered this pass is
	// meaningful only against an UNCHANGED version: a recording that changed
	// the procedure is part of the new version's corpus, not evidence about it.
	Lift          LiftOutcome
	ProcedureHash string
	Rung          work.Rung
}

// minedCorpus is the pipeline's winner and everything the lift needs beside
// it: the canonicalized corpus the occurrences index, and the recordings the
// corpus came from, in the same order.
type minedCorpus struct {
	win    proc.Accepted
	corpus [][]proc.Action
	recs   []recording
}

// runPipeline is the whole of epic C's algorithm side, and it reads as the
// record's numbered list because that IS the design:
//
//	Canonicalize -> Symbolize -> Mine -> Generalize -> Classify -> Score -> Select
//
// Structure runs beside Mine rather than after it: the tree is what tells a
// later reader that a pattern was a retry rather than a run of seven steps,
// and it is recorded on the result instead of being folded into the pattern.
func (i *Integration) runPipeline(ctx context.Context, k corpusKey) (LearnResult, *minedCorpus, error) {
	out := LearnResult{OwnerUserId: k.OwnerUserId, GoalSignature: k.GoalSignature, Level: k.Level}

	recs, err := i.loadCorpus(ctx, k)
	if err != nil {
		return out, nil, err
	}
	out.Sequences = len(recs)
	if len(recs) < 2 {
		// D14's floor, reached before any work is done. The record is
		// explicit that this is not a failure: "a corpus of one session
		// yields no procedure; the session is still lifted as a one-off
		// validated bundle, as today" -- which the existing capture path
		// does, untouched by this epic.
		out.Reason = "fewer than two recorded runs for this signature"
		return out, nil, nil
	}

	corpus := make([][]proc.Action, 0, len(recs))
	for _, rec := range recs {
		actions := proc.Canonicalize(rec.Steps)
		proc.SortActions(actions)
		corpus = append(corpus, actions)
	}

	// One symbol table over the WHOLE corpus, not one per run: a symbol that
	// meant different things in different runs could not be mined across
	// them, which is the only thing mining is for.
	flat, offsets := flatten(corpus)
	symbols := proc.Symbolize(flat, i.params)
	sequences := make([][]string, len(corpus))
	for r := range corpus {
		sequences[r] = sliceSymbols(flat, symbols, offsets[r], offsets[r]+len(corpus[r]))
	}

	// A LIKED RECORDING RANKS HIGHER AND IS STILL ONE USE (D23): its weight
	// breaks ties between patterns and never clears D14's floor, which counts
	// recordings. The occurrences keep sequence order, so a like changes which
	// pattern wins a tie and never the instances a winner is generalized from.
	weights := make([]float64, len(recs))
	for r, rec := range recs {
		weights[r] = float64(rec.weight())
	}
	patterns := proc.MineWeighted(sequences, weights, i.params.MinSupport, i.params.Gap)
	out.Candidates = len(patterns)
	if len(patterns) == 0 {
		out.Reason = "nothing recurred across the corpus"
		return out, nil, nil
	}

	candidates := make([]proc.Candidate, 0, len(patterns))
	for _, p := range patterns {
		instances := actionsOf(occurrenceInstances(p.Occurrences, corpus))
		if len(instances) < 2 {
			continue
		}
		tmpl := proc.Generalize(instances)
		tmpl.Holes = proc.Classify(tmpl, instances)
		tmpl.Holes = i.explainUnexplained(ctx, tmpl.Holes, instances)
		candidates = append(candidates, proc.Candidate{Template: tmpl, Occurrences: p.Occurrences})
	}

	accepted := proc.Select(candidates, corpus, i.params)
	if len(accepted) == 0 {
		out.Reason = "no abstraction cleared the compression floor"
		return out, nil, nil
	}
	out.Accepted = true
	return out, &minedCorpus{win: accepted[0], corpus: corpus, recs: recs}, nil
}

// explainUnexplained makes the ONE bounded model call of D6, and only for a
// hole Classify returned as unexplained -- a hole SOME instances derive and
// some do not.
//
// Three properties are load-bearing and each has a test.
//
// With no deriver installed this is a no-op, so the ordinary path reaches no
// provider at all. A hole with no derivation evidence never arrives here,
// because Classify calls it free: without that gate every free parameter would
// cost a call and "induction spends no model" would stop being true in
// practice.
//
// And whatever the model answers goes straight to CheckDerivation, which
// accepts it only if it holds on EVERY instance. A proposal explaining some
// instances is rejected and the hole stays free.
func (i *Integration) explainUnexplained(ctx context.Context, holes []proc.Hole, instances [][]proc.Action) []proc.Hole {
	if i.deriver == nil {
		return demoteUnexplained(holes)
	}
	out := make([]proc.Hole, 0, len(holes))
	asked := false
	for _, h := range holes {
		if h.Class != proc.HoleUnexplained || asked {
			out = append(out, demoteOne(h))
			continue
		}
		asked = true // BOUNDED: one call per template, which is what D6 says.
		expr, err := i.deriver.ProposeDerivation(ctx, h, instances)
		if err != nil || strings.TrimSpace(expr) == "" {
			i.log().Debug("procedure: no derivation proposed", "hole", h.Id, "err", err)
			out = append(out, demoteOne(h))
			continue
		}
		d, err := proc.ParseDerivation(expr)
		if err != nil {
			i.log().Debug("procedure: derivation refused by the grammar", "hole", h.Id, "expr", expr, "err", err)
			out = append(out, demoteOne(h))
			continue
		}
		ok, heldOn := proc.CheckDerivation(d, h, instances)
		if !ok {
			i.log().Info("procedure: derivation rejected",
				"hole", h.Id, "expr", expr, "heldOn", heldOn, "instances", len(instances))
			out = append(out, demoteOne(h))
			continue
		}
		h.Class = proc.HoleDataFlow
		h.Derivation = d.Expr
		h.Evidence = heldOn
		out = append(out, h)
	}
	return out
}

// demoteUnexplained turns every unexplained hole into a free parameter. It is
// what an unexplained hole MEANS once nobody is going to ask about it: the
// caller supplies the value.
func demoteUnexplained(holes []proc.Hole) []proc.Hole {
	out := make([]proc.Hole, 0, len(holes))
	for _, h := range holes {
		out = append(out, demoteOne(h))
	}
	return out
}

func demoteOne(h proc.Hole) proc.Hole {
	if h.Class == proc.HoleUnexplained {
		h.Class = proc.HoleFree
	}
	return h
}

// flatten concatenates the corpus into one action slice and records where each
// run started, so one symbol table covers every run.
func flatten(corpus [][]proc.Action) ([]proc.Action, []int) {
	var flat []proc.Action
	offsets := make([]int, len(corpus))
	for i, run := range corpus {
		offsets[i] = len(flat)
		flat = append(flat, run...)
	}
	return flat, offsets
}

func sliceSymbols(flat []proc.Action, symbols []proc.Symbol, from, to int) []string {
	all := proc.SymbolSequence(flat, symbols)
	if from > len(all) {
		return nil
	}
	if to > len(all) {
		to = len(all)
	}
	return append([]string(nil), all[from:to]...)
}

// instance is one occurrence of a pattern: the actions it names, and which
// corpus sequence -- which recording -- they came from.
type instance struct {
	seq     int
	actions []proc.Action
}

// occurrenceInstances lifts a pattern's occurrences back to the actions they
// name, keeping each one's recording. It is the ONE place instances are
// formed, for Generalize and for the lift's evidence alike: an expectation or
// a verdict read off a different instance set than the template was
// generalized from would describe some other procedure.
func occurrenceInstances(occs []proc.Occurrence, corpus [][]proc.Action) []instance {
	var out []instance
	for _, occ := range occs {
		if occ.Sequence >= len(corpus) {
			continue
		}
		run := corpus[occ.Sequence]
		acts := make([]proc.Action, 0, len(occ.Positions))
		for _, pos := range occ.Positions {
			if pos < len(run) {
				acts = append(acts, run[pos])
			}
		}
		if len(acts) == len(occ.Positions) {
			out = append(out, instance{seq: occ.Sequence, actions: acts})
		}
	}
	return out
}

func actionsOf(insts []instance) [][]proc.Action {
	out := make([][]proc.Action, len(insts))
	for i, in := range insts {
		out[i] = in.actions
	}
	return out
}

// --- capability handlers --------------------------------------------------

// LearnFromRun mines the corpus a finished run belongs to and lifts what it
// finds, at the action level -- the exported form of the learnFromRun
// builtin, for the proving driver and for Go callers. The caller must be the
// run's OWNER (a person, or server-side Go that borrowed them): the run is
// read through the owned read, and a procedure is filed in its owner's
// catalog.
//
// It does NOT run the shadow comparison the builtin runs after an unchanged
// lift: a Go caller compares with ShadowCompare, and a lift that compared as
// well would count the same recording twice.
func (i *Integration) LearnFromRun(ctx context.Context, runId string) (LearnResult, error) {
	return i.learnFromRun(ctx, strings.TrimSpace(runId), LevelAction)
}

func (i *Integration) handleLearnFromRun(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	runId := strings.TrimSpace(argString(args, "runId"))
	if runId == "" {
		return nil, fmt.Errorf("procedure.learnFromRun: runId is required")
	}
	ac, present := auth.AccessFromContext(ctx)
	if !present || ac == nil || ac.UserId == "" {
		return nil, fmt.Errorf("procedure.learnFromRun: an authenticated owner is required")
	}
	if ac.Synthetic {
		// Only the engine-owned completion trigger may borrow the event's one
		// named owner. The owner-filtered read in learnFromRun verifies that
		// hint; neither a caller-supplied automation nor an arbitrary system
		// actor gains a cross-owner lookup through this builtin.
		owner := strings.TrimSpace(argString(args, "ownerUserId"))
		if !auth.OriginFromContext(ctx).IsInternal() || ac.UserId != "system:automation:learnFromSucceededRun" || owner == "" {
			return nil, fmt.Errorf("procedure.learnFromRun: only the trusted completion trigger may supply an owner")
		}
		ctx = ownerActor(ctx, owner)
	}
	res, run, err := i.learnFromRunRow(ctx, runId, levelArg(args))
	if err != nil {
		return nil, err
	}
	reply := learnReply(res)
	// THE SHADOW COMPARISON, after the lift and only when it left the version
	// UNCHANGED (shadow.go): a recording that changed the procedure is part
	// of the new version's corpus, not evidence about it. A comparison that
	// fails is reported beside the lift rather than failing it -- the lift
	// happened, and the automation must not be retried into a second one.
	if res.Lift == LiftUnchanged && run != nil {
		outs, serr := i.shadowCompareRun(ctx, run)
		compared, matched := 0, 0
		for _, o := range outs {
			if o.NotCompared {
				// Not made on this node -- no evidence either way.
				continue
			}
			compared++
			if o.Match {
				matched++
			}
		}
		reply["shadowCompared"] = compared
		reply["shadowMatched"] = matched
		if serr != nil {
			i.log().Warn("procedure: the shadow comparison after a lift failed", "runId", runId, "error", serr)
			reply["shadowError"] = serr.Error()
		}
	}
	return i.reply(reply), nil
}

// learnFromRun is the handler body.
//
// THE GATE. @serverOnly is refused on a builtin at parse, so the check lives
// here (dsl/procedure/builtins.memql records why). The run is read through
// workRunForOwner under the CALLER's own actor -- a person, or the run's owner
// the completion trigger borrowed in handleLearnFromRun -- so a caller who
// does not own it reads zero rows, and the refusal below turns that silence
// into an answer. Every later read runs under the same owner.
func (i *Integration) learnFromRun(ctx context.Context, runId string, level Level) (LearnResult, error) {
	res, _, err := i.learnFromRunRow(ctx, runId, level)
	return res, err
}

// learnFromRunRow is learnFromRun, answering the run row it read beside the
// result -- the row the learn handler's shadow comparison is made over. The
// row is nil for an answer that learned nothing from it (a run that has not
// succeeded, a replay run, a run with no goal signature).
func (i *Integration) learnFromRunRow(ctx context.Context, runId string, level Level) (LearnResult, map[string]any, error) {
	res := LearnResult{RunId: runId, Level: level}
	if runId == "" {
		return res, nil, fmt.Errorf("procedure.learnFromRun: runId is required")
	}
	run, err := i.readRunForCaller(ctx, runId)
	if err != nil {
		return res, nil, err
	}
	if run == nil {
		return res, nil, fmt.Errorf("procedure.learnFromRun: run %s is not readable as the caller -- "+
			"learning runs on the owner's own corpus", runId)
	}
	owner := strings.TrimSpace(str(run, "ownerUserId"))
	if owner == "" {
		return res, nil, fmt.Errorf("procedure.learnFromRun: run %s has no owner, so there is nobody whose catalog a procedure could be filed in", runId)
	}
	if ac, ok := auth.AccessFromContext(ctx); !ok || ac == nil || strings.TrimSpace(ac.UserId) == "" || !sameUser(ac.UserId, owner) {
		// Belt and braces over the owned read: a cluster owner CAN read
		// another person's run, and a procedure learned from it would be
		// written into their catalog under their name.
		return res, nil, fmt.Errorf("procedure.learnFromRun: run %s belongs to another person; "+
			"a procedure is learned from its owner's corpus and filed in their catalog", runId)
	}
	res.OwnerUserId = owner
	if str(run, "status") != "succeeded" {
		res.Reason = "the run has not succeeded"
		return res, nil, nil
	}
	if isReplayRun(run) {
		// A procedure's own replay is never a recording (see corpus.go).
		// Answered rather than refused: the automation fires on every run
		// that succeeds, replays included, and this is the ordinary answer
		// for one.
		res.Reason = "a replay run is not a recording"
		return res, nil, nil
	}
	sig := strings.TrimSpace(str(run, "goalSignature"))
	if sig == "" {
		res.Reason = "the run carries no goalSignature, so there is no corpus to mine it against"
		return res, nil, nil
	}
	learned, err := i.learnAndPersist(ctx, corpusKey{OwnerUserId: owner, GoalSignature: sig, Level: level})
	learned.RunId = runId
	return learned, run, err
}

// readRunForCaller reads one run through the OWNED read, under whatever
// actor the caller carries. There is deliberately no cluster-wide variant:
// the completion trigger borrows the run's owner before it gets here.
func (i *Integration) readRunForCaller(ctx context.Context, runId string) (map[string]any, error) {
	rows, err := i.store.query(ctx, "query "+call("workRunForOwner", map[string]any{"runId": runId}))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

func learnReply(res LearnResult) map[string]any {
	return map[string]any{
		"runId":         res.RunId,
		"ownerUserId":   res.OwnerUserId,
		"goalSignature": res.GoalSignature,
		"level":         int(res.Level),
		"sequences":     res.Sequences,
		"candidates":    res.Candidates,
		"constructId":   res.ConstructId,
		"accepted":      res.Accepted,
		"reason":        res.Reason,
		"lift":          string(res.Lift),
		"procedureHash": res.ProcedureHash,
		"rung":          string(res.Rung),
	}
}

func (i *Integration) handleMineCorpus(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner := strings.TrimSpace(argString(args, "ownerUserId"))
	sig := strings.TrimSpace(argString(args, "goalSignature"))
	level := levelArg(args)
	ac, _ := auth.AccessFromContext(ctx)

	if owner == "" {
		// THE SWEEP (gap G10). A cron firing carries no arguments, so the
		// scheduled automation arrives with a blank owner -- and a blank owner
		// is answered by iterating every owner ONLY for the cluster's
		// maintenance principal (or trusted server-side Go running as the
		// cluster). From anybody else it is refused: a read with no actor
		// returns zero rows and no error, which would look exactly like a
		// corpus with nothing to learn.
		if !isClusterPrincipal(ac) {
			return nil, fmt.Errorf("procedure.mineCorpus: ownerUserId is required -- every read runs as that person, " +
				"and a blank one reads zero rows and no error, which looks exactly like a corpus with nothing to learn; " +
				"only the scheduled sweep, running as the cluster's maintenance principal, mines every owner")
		}
		return i.reply(i.mineEveryOwner(ctx, sig, level)), nil
	}
	// A person mines their OWN corpus. The cluster may name any owner, and
	// so may server-side Go that carries no caller at all; a synthetic actor
	// that is NOT the cluster -- an ordinary automation's reader -- may not.
	if ac != nil && !isClusterPrincipal(ac) && !sameUser(ac.UserId, owner) {
		return nil, fmt.Errorf("procedure.mineCorpus: a person mines their own corpus; " +
			"the fleet-wide sweep is the scheduled automation, not this call")
	}
	signatures := []string{sig}
	if sig == "" {
		var err error
		if signatures, err = i.ownerSignatures(ctx, owner); err != nil {
			return nil, err
		}
	}
	summary := newMineSummary(owner, sig, level)
	for _, s := range signatures {
		res, err := i.learnAndPersist(ctx, corpusKey{OwnerUserId: owner, GoalSignature: s, Level: level})
		if err != nil {
			return nil, err
		}
		summary.add(res)
	}
	return i.reply(summary.reply(len(signatures))), nil
}

// mineEveryOwner is the sweep: every owner, every signature (or the one
// named), each mined under that owner's own actor. One owner's failure is
// logged and skipped -- a sweep that stops at the first unreadable corpus
// learns nothing for everybody after it.
func (i *Integration) mineEveryOwner(ctx context.Context, sig string, level Level) map[string]any {
	summary := newMineSummary("", sig, level)
	owners, err := i.ownerIds(ctx)
	if err != nil {
		i.log().Warn("procedure: the corpus sweep could not list owners", "error", err)
		out := summary.reply(0)
		out["reason"] = "the owners could not be listed: " + err.Error()
		return out
	}
	signatures := 0
	for _, owner := range owners {
		sigs := []string{sig}
		if sig == "" {
			if sigs, err = i.ownerSignatures(ctx, owner); err != nil {
				i.log().Warn("procedure: the corpus sweep could not read an owner's goals", "owner", owner, "error", err)
				continue
			}
		}
		for _, s := range sigs {
			signatures++
			res, err := i.learnAndPersist(ctx, corpusKey{OwnerUserId: owner, GoalSignature: s, Level: level})
			if err != nil {
				i.log().Warn("procedure: the corpus sweep could not mine one corpus", "owner", owner, "goalSignature", s, "error", err)
				continue
			}
			summary.add(res)
		}
	}
	out := summary.reply(signatures)
	out["owners"] = len(owners)
	return out
}

// ownerIds lists every active person the sweep mines for, each once, sorted
// -- the store's activeUserIds, which only the cluster's principal reaches.
func (i *Integration) ownerIds(ctx context.Context) ([]string, error) {
	rows, err := i.store.activeUserIds(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range rows {
		id := strings.TrimSpace(str(r, "id"))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// maxSignatureRuns bounds how many of one owner's runs ownerSignatures reads.
// The read is newest first, so an owner whose history outgrows the bound keeps
// their recent goals -- where recurrence is, and all the corpus read itself
// looks at -- and the stop is LOGGED, because a read that ended at a limit must
// not look like a history that ended there.
const maxSignatureRuns = 10000

// ownerSignatures is every goal signature one owner has a SUCCEEDED recording
// for, read under that owner's actor across EVERY PAGE of their runs, newest
// first, up to maxSignatureRuns. One page was the engine's default window --
// the newest few hundred runs -- so a goal last recorded further back than
// that was never mined again. A replay run is not a recording and names no
// corpus of its own.
func (i *Integration) ownerSignatures(ctx context.Context, owner string) ([]string, error) {
	actorCtx := ownerActor(ctx, owner)
	seen := map[string]bool{}
	var out []string
	cursor, read := "", 0
	for {
		rows, next, err := i.store.queryPage(actorCtx, cursor, "query "+call("workRunsForOwner", nil))
		if err != nil {
			return nil, err
		}
		read += len(rows)
		for _, r := range rows {
			s := strings.TrimSpace(str(r, "goalSignature"))
			if s == "" || seen[s] || str(r, "status") != "succeeded" || isReplayRun(r) {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
		if next == "" || next == cursor || len(rows) == 0 {
			break
		}
		if read >= maxSignatureRuns {
			i.log().Warn("procedure: the corpus sweep stopped reading an owner's runs at its bound; goals recorded only before them are not mined this pass",
				"owner", owner, "runsRead", read, "bound", maxSignatureRuns, "signatures", len(out))
			break
		}
		cursor = next
	}
	sort.Strings(out)
	return out, nil
}

// mineSummary folds several mining passes into the one reply a capability
// returns.
type mineSummary struct {
	owner, sig    string
	level         Level
	sequences     int
	candidates    int
	lifted        []string
	lastReason    string
	lastConstruct string
}

func newMineSummary(owner, sig string, level Level) *mineSummary {
	return &mineSummary{owner: owner, sig: sig, level: level}
}

func (s *mineSummary) add(res LearnResult) {
	s.sequences += res.Sequences
	s.candidates += res.Candidates
	if res.ConstructId != "" {
		s.lifted = append(s.lifted, res.ConstructId)
		s.lastConstruct = res.ConstructId
	}
	if res.Reason != "" {
		s.lastReason = res.Reason
	}
}

func (s *mineSummary) reply(signatures int) map[string]any {
	out := map[string]any{
		"ownerUserId":   s.owner,
		"goalSignature": s.sig,
		"level":         int(s.level),
		"signatures":    signatures,
		"sequences":     s.sequences,
		"candidates":    s.candidates,
		"constructId":   s.lastConstruct,
		"constructIds":  append([]string{}, s.lifted...),
		"accepted":      len(s.lifted) > 0,
		"reason":        s.lastReason,
	}
	if signatures == 0 && s.lastReason == "" {
		out["reason"] = "no goal signature had a recorded corpus"
	}
	return out
}

// isClusterPrincipal reports the cluster acting: a SYNTHETIC actor with the
// cluster-owner role -- the maintenance principal a listed automation runs
// under, or a system actor trusted server-side Go builds. An ordinary
// automation's synthetic READER is not one, and neither is any person.
func isClusterPrincipal(ac *auth.AccessContext) bool {
	return ac != nil && ac.Synthetic && ac.IsClusterOwner()
}

// sameUser compares two user ids that may be spelled canonically
// (v1:identity:user:<id>) or bare -- a run row stores the canonical form of a
// relationship field, and a caller's token carries whichever the identity
// service minted.
func sameUser(a, b string) bool {
	return bareUserId(a) != "" && bareUserId(a) == bareUserId(b)
}

func bareUserId(id string) string {
	return strings.TrimPrefix(strings.TrimSpace(id), "v1:identity:user:")
}

// levelArg reads D24's corpus level. The narrowing takes the CALLER'S DEFAULT
// answer, which here is LevelAction: an unreadable or out-of-range level must
// mine the actions rather than mine nothing, because a sweep that silently
// mines an empty level looks exactly like a corpus with nothing left to learn.
func levelArg(args map[string]any) Level {
	switch v := args["level"].(type) {
	case float64:
		if num.Float64Or(v, int(LevelAction)) == int(LevelAutomation) {
			return LevelAutomation
		}
	case int:
		if v == int(LevelAutomation) {
			return LevelAutomation
		}
	}
	return LevelAction
}

func argString(args map[string]any, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// reply is the one-row answer shape every capability returns. It is never
// persisted: the reply is a value the caller reads, the same shape every other
// integration answers with.
func (i *Integration) reply(payload map[string]any) []memorynodes.MemoryNode {
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte("{}")
	}
	at := i.clock().UTC()
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("procedure:%d", at.UnixNano()),
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: at,
		Payload:   raw,
	}}
}
