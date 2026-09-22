package procedure

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/core/num"
)

// learn.go -- the pipeline, in the record's order, and the two capability
// handlers over it.

// learnResult is what one mining pass produced.
type learnResult struct {
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
}

// runPipeline is the whole of epic C's algorithm side, and it reads as the
// record's numbered list because that IS the design:
//
//	Canonicalize -> Symbolize -> Mine -> Generalize -> Classify -> Score -> Select
//
// Structure runs beside Mine rather than after it: the tree is what tells a
// later reader that a pattern was a retry rather than a run of seven steps,
// and it is recorded on the result instead of being folded into the pattern.
func (i *Integration) runPipeline(ctx context.Context, k corpusKey) (learnResult, []proc.Accepted, [][]proc.Action, []string, error) {
	out := learnResult{OwnerUserId: k.OwnerUserId, GoalSignature: k.GoalSignature, Level: k.Level}

	runs, runIds, err := i.loadCorpus(ctx, k)
	if err != nil {
		return out, nil, nil, nil, err
	}
	out.Sequences = len(runs)
	if len(runs) < 2 {
		// D14's floor, reached before any work is done. The record is
		// explicit that this is not a failure: "a corpus of one session
		// yields no procedure; the session is still lifted as a one-off
		// validated bundle, as today" -- which the existing capture path
		// does, untouched by this epic.
		out.Reason = "fewer than two recorded runs for this signature"
		return out, nil, nil, nil, nil
	}

	corpus := make([][]proc.Action, 0, len(runs))
	for _, steps := range runs {
		actions := proc.Canonicalize(steps)
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

	patterns := proc.Mine(sequences, i.params)
	out.Candidates = len(patterns)
	if len(patterns) == 0 {
		out.Reason = "nothing recurred across the corpus"
		return out, nil, corpus, runIds, nil
	}

	candidates := make([]proc.Candidate, 0, len(patterns))
	for _, p := range patterns {
		instances := instancesOf(p, corpus)
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
		return out, nil, corpus, runIds, nil
	}
	out.Accepted = true
	return out, accepted, corpus, runIds, nil
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

// instancesOf lifts a pattern's occurrences back to the actions they name.
func instancesOf(p proc.Pattern, corpus [][]proc.Action) [][]proc.Action {
	var out [][]proc.Action
	for _, occ := range p.Occurrences {
		if occ.Sequence >= len(corpus) {
			continue
		}
		run := corpus[occ.Sequence]
		inst := make([]proc.Action, 0, len(occ.Positions))
		for _, pos := range occ.Positions {
			if pos < len(run) {
				inst = append(inst, run[pos])
			}
		}
		if len(inst) == len(occ.Positions) {
			out = append(out, inst)
		}
	}
	return out
}

// runIsDisliked reports whether any feedback observation on the run carries a
// negative verdict (D23). A disliked recording is EXCLUDED from the corpus
// entirely rather than weighted to zero, because a template generalized from
// a run somebody rejected is a template that reproduces what they rejected.
func (i *Integration) runIsDisliked(ctx context.Context, runId string) (bool, error) {
	rows, err := i.store.query(ctx, "query "+call("workObservationsForOwnerRun", map[string]any{"runId": runId}))
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if str(r, "kind") != "feedback" {
			continue
		}
		if str(obj(r, "data"), "verdict") == "disliked" {
			return true, nil
		}
	}
	return false, nil
}

// --- capability handlers --------------------------------------------------

func (i *Integration) handleLearnFromRun(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	runId := strings.TrimSpace(argString(args, "runId"))
	if runId == "" {
		return nil, fmt.Errorf("procedure.learnFromRun: runId is required")
	}
	run, err := i.runById(ctx, runId)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("procedure.learnFromRun: run %s not found, or not readable as its owner", runId)
	}
	k := corpusKey{
		OwnerUserId:   str(run, "ownerUserId"),
		GoalSignature: str(run, "goalSignature"),
		Level:         levelArg(args),
	}
	if k.GoalSignature == "" {
		return i.reply(map[string]any{
			"runId":    runId,
			"accepted": false,
			"reason":   "the run carries no goalSignature, so there is no corpus to mine it against",
		}), nil
	}
	res, err := i.learnAndPersist(ctx, k)
	if err != nil {
		return nil, err
	}
	return i.reply(map[string]any{
		"runId":         runId,
		"goalSignature": res.GoalSignature,
		"sequences":     res.Sequences,
		"candidates":    res.Candidates,
		"constructId":   res.ConstructId,
		"accepted":      res.Accepted,
		"reason":        res.Reason,
	}), nil
}

func (i *Integration) handleMineCorpus(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	owner := strings.TrimSpace(argString(args, "ownerUserId"))
	if owner == "" {
		return nil, fmt.Errorf("procedure.mineCorpus: ownerUserId is required -- every read runs as that person")
	}
	k := corpusKey{
		OwnerUserId:   owner,
		GoalSignature: strings.TrimSpace(argString(args, "goalSignature")),
		Level:         levelArg(args),
	}
	res, err := i.learnAndPersist(ctx, k)
	if err != nil {
		return nil, err
	}
	return i.reply(map[string]any{
		"ownerUserId":   owner,
		"goalSignature": res.GoalSignature,
		"level":         int(res.Level),
		"sequences":     res.Sequences,
		"candidates":    res.Candidates,
		"constructId":   res.ConstructId,
		"accepted":      res.Accepted,
		"reason":        res.Reason,
	}), nil
}

func (i *Integration) runById(ctx context.Context, runId string) (map[string]any, error) {
	rows, err := i.store.query(ctx, "query "+call("workRunById", map[string]any{"runId": runId}))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
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
