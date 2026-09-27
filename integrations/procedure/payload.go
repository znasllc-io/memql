package procedure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/component/work"
)

// payload.go -- what the lift STORES on a learned procedure's construct, and
// the one way to read it back (epic memql#5408, plan section 1.3).
//
// Three fields travel together and are pinned together:
//
//	source         the rendered MemQL -- the auditable artifact a person reads
//	procedure      what a replay EXECUTES: the template, its expectations, its
//	               footprint and the procedure's own process model
//	preconditions  the learned initiation set (component/procedure)
//
// procedureHash is the VERSION: the value a procedurePromotion approval pins
// as its artifact hash. A person approves a version, and a later change to any
// of the three is a new version -- a new candidate that has to earn its
// promotion again (D15).
//
// THE HASH IS OVER THE PROCEDURE'S BEHAVIOUR, NOT ITS PROVENANCE, and that is
// the one place it departs from "sha256 over the three". Every recording that
// succeeds re-runs the lift over a corpus one run larger, and a recording that
// AGREES with the procedure changes nothing a replay does -- but it does add
// a run id, a session id and a use to the provenance. Hashed, the provenance
// would make every agreeing recording a new version, reset the ladder, and a
// shadow streak could never reach m. So two things are left out of what is
// hashed, and each is stated where it is stored:
//
//   - the source's COMMENT lines, which carry D9's provenance stamp and the
//     description;
//   - procedure.recordedFrom, the same stamp as data, and procedure.title,
//     the goal statement the description quotes -- read off the oldest
//     recording the corpus still holds, so it can be reworded when that one
//     ages out, with nothing a replay does changing.
//
// All of them are still STORED, and they describe the version: the lift
// writes nothing when the hash is unchanged, so they name the recordings the
// version was first learned from. Anyone can recompute the hash from the
// stored row with procedureHash below.
//
// A THIRD THING IS LEFT OUT, and it is not provenance: procedure.hints, what
// a dispatcher is told BESIDE a step's template (review finding I2). Today it
// is one value, hints.timeoutsMs -- per step, the longest time limit any
// recording's call asked for, 0 where none did. The app picks a different
// limit nearly every call, so it cannot be a parameter (semantic.go drops it
// from the arguments), and no replay's behaviour is judged by it: a replay is
// handed it as DispatchRequest.Timeout, so a command the app let run for five
// minutes is not cut off at the executor's default. Being outside the hash, a
// longer limit a later recording asks for is the one thing a kept version
// rewrites (persist.go's keepProcedure) -- the version, its ladder and its
// provenance stay as they are.

// procedurePayloadVersion is the payload's `v`.
const procedurePayloadVersion = 1

// Procedure is construct.procedure. Its JSON keys are the contract a replay,
// the OS and the proving driver read:
//
//	{v, level, title, goalSignature, inputKeys, steps: [{tool, args, symbol}],
//	 holes, expect, inputMap, freeParameters, footprint, target,
//	 symbols: [{id, tool, template}], model,
//	 recordedFrom: {app, model, effort, sessionIds, runIds, workspaces},
//	 hints: {timeoutsMs}}
type Procedure struct {
	V int `json:"v"`
	// Level is the CORPUS level the procedure was mined at: 1 for the actions
	// inside recorded app sessions, 2 for automation invocations (design
	// D24). It is not an AI level -- the steps of a level-2 procedure are
	// automation calls, which no action dispatcher runs.
	Level int `json:"level"`
	// Title is the goal statement the recordings served, read from the
	// oldest recording's goal; empty when none could be read.
	Title string `json:"title"`
	// GoalSignature is component/work.GoalSignature of that goal, and
	// InputKeys are its sorted input names -- the two things the signature
	// was computed over.
	GoalSignature string   `json:"goalSignature"`
	InputKeys     []string `json:"inputKeys"`
	// Steps are the template's steps; Holes classify every open position.
	Steps []ProcedureStep `json:"steps"`
	Holes []proc.Hole     `json:"holes"`
	// Expect holds one expectation per step: what every recording of that
	// step agreed on, in executor-independent terms. A content path is
	// relative to the recording's workspace when the file was inside it.
	Expect []work.StepExpectation `json:"expect"`
	// InputMap maps a FREE hole id to the goal input key that supplied it in
	// every recording (component/procedure.LearnInputMap); FreeParameters are
	// every free hole id, sorted. A free hole with no input key cannot be
	// bound from a goal, and a replay refuses to start without it.
	InputMap       map[string]string `json:"inputMap"`
	FreeParameters []string          `json:"freeParameters"`
	// Footprint is the union of every recorded action's footprint, measured
	// against its recording's workspace; Target is where it replays (D4).
	Footprint work.Footprint `json:"footprint"`
	Target    string         `json:"target"`
	// Symbols are one per step (id s<i>, the step's template), and Model is
	// the procedure's own process model: the sequence of those symbols. A
	// replay's live trace is checked against Model with
	// component/procedure.PrefixFits.
	Symbols []proc.Symbol     `json:"symbols"`
	Model   *proc.ProcessTree `json:"model"`
	// RecordedFrom is D9's provenance. It is NOT hashed; see the file header.
	RecordedFrom RecordedFrom `json:"recordedFrom"`
	// Hints are what a dispatcher is told beside a step's template. NOT
	// hashed; see the file header.
	Hints *ProcedureHints `json:"hints,omitempty"`
}

// ProcedureHints are values the recordings varied in that no goal input
// supplies and no replay's behaviour is judged by, kept beside the template
// rather than in it.
type ProcedureHints struct {
	// TimeoutsMs is, per step, the LONGEST per-call timeout any recording of
	// that step asked for, in milliseconds; 0 where none recorded one.
	TimeoutsMs []int `json:"timeoutsMs,omitempty"`
}

// ProcedureStep is one step: the tool, its argument template, and its symbol.
type ProcedureStep struct {
	Tool   string     `json:"tool"`
	Args   *proc.Node `json:"args"`
	Symbol string     `json:"symbol"`
}

// RecordedFrom is where a procedure came from. App, Model and Effort are what
// every recording agreed on and are ABSENT when the recordings disagreed or
// the app reported nothing -- a value borrowed from one recording would claim
// something about the others. Workspaces are the recordings' working
// directories, which is what a dispatcher rebases a recorded absolute path
// inside a workspace against.
type RecordedFrom struct {
	App        string   `json:"app,omitempty"`
	Model      string   `json:"model,omitempty"`
	Effort     string   `json:"effort,omitempty"`
	SessionIds []string `json:"sessionIds"`
	RunIds     []string `json:"runIds"`
	Workspaces []string `json:"workspaces,omitempty"`
}

// Template is the procedure's steps and holes as the component/procedure
// value Bind and Materialize take.
func (p Procedure) Template() proc.Template {
	t := proc.Template{Steps: make([]proc.TemplateStep, len(p.Steps)), Holes: append([]proc.Hole(nil), p.Holes...)}
	for i, s := range p.Steps {
		t.Steps[i] = proc.TemplateStep{Tool: s.Tool, Args: s.Args}
	}
	return t
}

// DecodeProcedure reads construct.procedure back from a row, in whichever
// shape it arrives: a decoded object (what a query answers), raw JSON, or the
// Go value itself. Every node and tree is CHECKED on the way in -- an unknown
// kind, form or operator is refused rather than read as something close to it
// -- because the caller is about to execute what this returns.
func DecodeProcedure(v any) (Procedure, error) {
	var p Procedure
	if err := decodeInto(v, &p); err != nil {
		return Procedure{}, fmt.Errorf("procedure: decoding a stored procedure: %w", err)
	}
	if p.V != procedurePayloadVersion {
		return Procedure{}, fmt.Errorf("procedure: stored procedure version %d is not %d", p.V, procedurePayloadVersion)
	}
	return p, nil
}

// DecodePreconditions reads construct.preconditions back. An absent value is
// the empty set -- nothing was learned -- and not an error.
func DecodePreconditions(v any) (proc.Preconditions, error) {
	var p proc.Preconditions
	if v == nil {
		return p, nil
	}
	if err := decodeInto(v, &p); err != nil {
		return proc.Preconditions{}, fmt.Errorf("procedure: decoding stored preconditions: %w", err)
	}
	return p, nil
}

func decodeInto(v any, into any) error {
	var raw []byte
	switch t := v.(type) {
	case nil:
		return fmt.Errorf("nothing is stored")
	case []byte:
		raw = t
	case json.RawMessage:
		raw = t
	case string:
		raw = []byte(t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return err
		}
		raw = b
	}
	return json.Unmarshal(raw, into)
}

// asObject turns a Go value into the decoded-JSON object the engine stores
// and the call renderer writes -- one encoding, the one every reader decodes.
func asObject(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// procedureHash is the version a procedurePromotion approval pins:
// component/procedure.Digest over the CANONICAL JSON (keys sorted, no
// insignificant whitespace, nothing HTML-escaped) of
//
//	{source: <source without its comment lines>,
//	 procedure: <procedure without recordedFrom and title>,
//	 preconditions: <preconditions>}
//
// The exclusions are the provenance and the presentation, and the file header
// says why neither can be hashed. Everything a replay executes or checks is
// inside it.
func procedureHash(source string, procedure, preconditions map[string]any) (string, error) {
	behaviour := make(map[string]any, len(procedure))
	for k, v := range procedure {
		if unhashedProcedureKeys[k] {
			continue
		}
		behaviour[k] = v
	}
	if preconditions == nil {
		preconditions = map[string]any{}
	}
	canonical, err := canonicalJSON(map[string]any{
		"source":        sourceWithoutComments(source),
		"procedure":     behaviour,
		"preconditions": preconditions,
	})
	if err != nil {
		return "", err
	}
	return proc.Digest(canonical), nil
}

// unhashedProcedureKeys are the payload keys outside the version: provenance,
// presentation and the dispatch hints -- never behaviour.
var unhashedProcedureKeys = map[string]bool{"recordedFrom": true, "title": true, "hints": true}

// canonicalJSON encodes with sorted keys (encoding/json sorts map keys), no
// indentation and no HTML escaping -- `<` written as < is the same
// document and a different digest, and the reader recomputing this should not
// have to know which one we chose.
func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// sourceWithoutComments drops every line that is a comment. A statement line
// cannot begin with `//` -- a string literal never spans lines -- so what is
// dropped is exactly the provenance stamp and the note an unwritten step
// leaves, both of which the procedure payload states as data.
func sourceWithoutComments(source string) string {
	lines := strings.Split(source, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}

// buildProcedure assembles the payload from the winning template and the
// instances it was generalized from, and learns the preconditions from the
// recordings those instances came from. Reads (the goal statement) run under
// the owner's actor already in ctx.
//
// Every per-step value is read off the SAME instances Generalize saw
// (occurrenceInstances), in the same order: an expectation or an input map
// learned from a different set of recordings would describe some other
// procedure.
func (i *Integration) buildProcedure(ctx context.Context, k corpusKey, t proc.Template, insts []instance, recs []recording) (Procedure, proc.Preconditions) {
	p := Procedure{
		V:              procedurePayloadVersion,
		Level:          int(k.Level),
		GoalSignature:  k.GoalSignature,
		InputKeys:      []string{},
		Steps:          make([]ProcedureStep, 0, len(t.Steps)),
		Holes:          make([]proc.Hole, 0, len(t.Holes)),
		Expect:         make([]work.StepExpectation, 0, len(t.Steps)),
		FreeParameters: []string{},
		Symbols:        make([]proc.Symbol, 0, len(t.Steps)),
	}

	// ONE SYMBOL PER STEP, named by position. The corpus-wide symbols
	// Symbolize produced are renumbered and re-widened by every recording that
	// joins the corpus, and a payload holding them would be a new version on
	// every recording that changed nothing a replay does. A step's own
	// template is fixed by the version, and it is what AssignSymbol measures a
	// recorded action against.
	ids := make([]string, len(t.Steps))
	for j, s := range t.Steps {
		ids[j] = fmt.Sprintf("s%d", j)
		p.Steps = append(p.Steps, ProcedureStep{Tool: s.Tool, Args: s.Args, Symbol: ids[j]})
		p.Symbols = append(p.Symbols, proc.Symbol{Id: ids[j], Tool: s.Tool, Template: s.Args})
	}
	p.Model = proc.SequenceTree(ids)

	// A hole's Evidence counts the instances its class held on -- which is
	// the size of the corpus, not a property of the procedure, and would make
	// every agreeing recording a new version. The class is what is stored.
	for _, h := range t.Holes {
		h.Evidence = 0
		p.Holes = append(p.Holes, h)
		if h.Class == proc.HoleFree {
			p.FreeParameters = append(p.FreeParameters, h.Id)
		}
	}
	sort.Strings(p.FreeParameters)

	for j := range t.Steps {
		observed := make([]work.StepObservation, 0, len(insts))
		for _, in := range insts {
			if j < len(in.actions) {
				observed = append(observed, recs[in.seq].Evidence[in.actions[j].Key].Observation)
			}
		}
		p.Expect = append(p.Expect, work.ExpectationFrom(observed))
	}

	// THE LONGEST TIME LIMIT any instance of a step asked for, as a hint
	// beside the template (the file header says why it is not hashed).
	timeouts := make([]int, len(t.Steps))
	anyTimeout := false
	for j := range t.Steps {
		for _, in := range insts {
			if j >= len(in.actions) {
				continue
			}
			if ms := recs[in.seq].Evidence[in.actions[j].Key].TimeoutMs; ms > timeouts[j] {
				timeouts[j], anyTimeout = ms, true
			}
		}
	}
	if anyTimeout {
		p.Hints = &ProcedureHints{TimeoutsMs: timeouts}
	}

	inputs := make([]map[string]any, len(insts))
	for n, in := range insts {
		inputs[n] = recs[in.seq].variables()
		if inputs[n] == nil {
			inputs[n] = map[string]any{}
		}
	}
	p.InputMap = proc.LearnInputMap(t, actionsOf(insts), inputs)
	if p.InputMap == nil {
		p.InputMap = map[string]string{}
	}

	// THE FOOTPRINT is measured per recording, against THAT recording's
	// workspace (the fingerprint's cwd, not a directory a command moved to):
	// a path inside the workspace a session ran in is portable, and the same
	// path read against another session's workspace would not be. It is the
	// workspace the action's arguments were relativized against, so a path
	// the rewrite made relative and one it left absolute are judged alike.
	for _, in := range insts {
		rec := recs[in.seq]
		for _, a := range in.actions {
			ev := rec.Evidence[a.Key]
			ws := ev.Workspace
			if ws == "" {
				ws = rec.Workspace
			}
			p.Footprint = unionFootprint(p.Footprint, work.ActionFootprint(a.Tool, ws, ev.Paths))
		}
	}
	p.Target = string(work.ReplayTargetFor(p.Footprint))

	// The recordings behind the instances, each once, oldest first.
	var (
		seqs         []int
		seen         = map[int]bool{}
		fingerprints []map[string]any
	)
	for _, in := range insts {
		if !seen[in.seq] {
			seen[in.seq] = true
			seqs = append(seqs, in.seq)
		}
	}
	sort.Ints(seqs)
	var apps, models, efforts []string
	sessions, workspaces := map[string]bool{}, map[string]bool{}
	p.RecordedFrom.SessionIds, p.RecordedFrom.RunIds = []string{}, []string{}
	for _, s := range seqs {
		rec := recs[s]
		p.RecordedFrom.RunIds = append(p.RecordedFrom.RunIds, rec.RunId)
		fingerprints = append(fingerprints, rec.Fingerprint)
		input, summary := obj(rec.Run, "input"), obj(rec.Run, "summary")
		apps = append(apps, strings.TrimSpace(str(input, "app")))
		models = append(models, strings.TrimSpace(str(summary, "model")))
		efforts = append(efforts, strings.TrimSpace(str(summary, "effort")))
		for _, sid := range []string{str(summary, "sessionId"), str(input, "sessionId")} {
			if sid = strings.TrimSpace(sid); sid != "" {
				sessions[sid] = true
			}
		}
		if rec.Workspace != "" {
			workspaces[rec.Workspace] = true
		}
		if len(p.InputKeys) == 0 {
			if vars := rec.variables(); len(vars) > 0 {
				for key := range vars {
					p.InputKeys = append(p.InputKeys, key)
				}
				sort.Strings(p.InputKeys)
			}
		}
		if p.Title == "" {
			p.Title = i.goalStatement(ctx, rec.Run)
		}
	}
	p.RecordedFrom.App = agreed(apps)
	p.RecordedFrom.Model = agreed(models)
	p.RecordedFrom.Effort = agreed(efforts)
	p.RecordedFrom.SessionIds = sortedKeys(sessions)
	if len(workspaces) > 0 {
		p.RecordedFrom.Workspaces = sortedKeys(workspaces)
	}

	return p, proc.LearnPreconditions(fingerprints, proc.UsedTools(t))
}

// goalStatement is the statement of the goal a recording served: the goal of
// the run that DELEGATED it when it names one (a session's own run belongs to
// a bookkeeping goal -- "run this step in claude-code"), its own otherwise.
// Empty when neither can be read, which the payload keeps as empty rather than
// as a guess.
func (i *Integration) goalStatement(ctx context.Context, run map[string]any) string {
	goalRun := run
	if parent := strings.TrimSpace(str(run, "parentRunId")); parent != "" {
		rows, err := i.store.query(ctx, "query "+call("workRunForOwner", map[string]any{"runId": parent}))
		if err != nil || len(rows) == 0 {
			return ""
		}
		goalRun = rows[0]
	}
	goalId := strings.TrimSpace(str(goalRun, "goalId"))
	if goalId == "" {
		return ""
	}
	rows, err := i.store.query(ctx, "query "+call("workGoalForOwner", map[string]any{"goalId": goalId}))
	if err != nil || len(rows) == 0 {
		return ""
	}
	return strings.TrimSpace(str(rows[0], "statement"))
}

// candidateEvidence is what the candidate gate reads (component/work,
// D15 and D23): the uses, the holes nobody could classify, and a person's
// verdicts on every instance step, versions oldest first.
func candidateEvidence(win proc.Accepted, t proc.Template, insts []instance, recs []recording) work.CandidateEvidence {
	ev := work.CandidateEvidence{Uses: win.Utility.Uses}
	for _, h := range t.Holes {
		if h.Class == proc.HoleUnexplained {
			ev.UnexplainedHoles++
		}
	}
	for _, in := range insts {
		rec := recs[in.seq]
		row := make([]work.StepVersions, len(in.actions))
		for j, a := range in.actions {
			row[j] = stepVersions(rec.Verdicts[a.Key])
		}
		ev.Instances = append(ev.Instances, row)
	}
	return ev
}

// unionFootprint folds one footprint into another: every flag OR-ed, the
// concepts a sorted set.
func unionFootprint(a, b work.Footprint) work.Footprint {
	out := work.Footprint{
		Files:    a.Files || b.Files,
		Machine:  a.Machine || b.Machine,
		External: a.External || b.External,
		Spend:    a.Spend || b.Spend,
	}
	set := map[string]bool{}
	for _, c := range append(append([]string(nil), a.Concepts...), b.Concepts...) {
		if c != "" {
			set[c] = true
		}
	}
	if len(set) > 0 {
		out.Concepts = sortedKeys(set)
	}
	return out
}

// agreed is the value every recording reported, or "" when they disagreed or
// none reported one.
func agreed(values []string) string {
	first := ""
	for _, v := range values {
		switch {
		case v == "":
			return ""
		case first == "":
			first = v
		case v != first:
			return ""
		}
	}
	return first
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
