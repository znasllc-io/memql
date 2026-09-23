package procedure

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/planner"
)

// persist.go -- the winner as a construct: rendered, compiled, stored,
// versioned, and put on the certification ladder.
//
// THE LIFT IS IDEMPOTENT (gap G15). It is keyed by the procedure's NAME, which
// is stable for one (owner, goal signature, level), and by the procedure's
// VERSION (procedureHash, payload.go). Three answers, each for a reason:
//
//	absent             create it, and put it on the ladder's entry rung
//	same version       write NOTHING -- a re-mine of a corpus that taught the
//	                   procedure nothing new is free, and keeps every counter
//	                   the ladder has earned
//	different version  write the new version IN PLACE and put it back on the
//	                   entry rung: a person approved the old one, and "a later
//	                   change of the construct is a new candidate" (D15)
//
// Before this, every mining pass minted a fresh construct under the same name,
// so a procedure could never accumulate the shadow evidence it is promoted on.
//
// THE ORDER OF THE WRITES IS THE DESIGN.
//
//   - goalSignature is written LAST on a create. It is the key compile's exact
//     tier finds a procedure by, so a signature written before the rest would
//     point a goal at a construct that is not yet what it claims to be.
//   - On a re-lift the LADDER is written FIRST. The construct already carries
//     its signature and may be trusted -- served with no model -- and a new
//     version written before the rung came down would be served unreviewed
//     for as long as the two writes took. Rung first, then the payload.
//
// NOTHING AUTO-ACTIVATES. The bundle is recorded `validated` and the construct
// stays `draft`: a learned procedure is served through the ladder, never as an
// ordinary automation, and its source is the artifact a person approves.

// compileEngine is the legacy Gate 1 seam: an engine that compiles bundles
// itself. The production seam is SetCompiler (gap G7); this stays for a test
// engine that carries both.
type compileEngine interface {
	CompileBundle(constructs []memql.SandboxConstruct) memql.SandboxReport
}

// learnAndPersist runs the pipeline and, when an abstraction clears the floor,
// lifts the best one.
func (i *Integration) learnAndPersist(ctx context.Context, k corpusKey) (LearnResult, error) {
	res, mined, err := i.runPipeline(ctx, k)
	if err != nil {
		return res, err
	}
	if !res.Accepted || mined == nil {
		return res, nil
	}
	lifted, err := i.lift(ctx, k, mined)
	if err != nil {
		return res, err
	}
	res.ConstructId = lifted.constructId
	res.Lift = lifted.outcome
	res.ProcedureHash = lifted.hash
	res.Rung = lifted.rung
	return res, nil
}

// lifted is what one lift did.
type lifted struct {
	constructId string
	outcome     LiftOutcome
	hash        string
	rung        work.Rung
}

// version is everything a lift computes BEFORE it writes anything: the
// rendered source, the payload, the hash that names the version, Gate 1's
// verdict and the rung the version enters on. Computing it whole first is
// what lets the lift decide create / keep / re-lift without a write that has
// to be undone.
type version struct {
	name          string
	title         string
	source        string
	procedure     map[string]any
	preconditions map[string]any
	hash          string
	report        memql.SandboxReport
	gateRan       bool
	reRunnable    bool
	rung          work.Rung
	reason        string
	uses          int
	firstRunId    string
}

func (i *Integration) lift(ctx context.Context, k corpusKey, mined *minedCorpus) (lifted, error) {
	owner := strings.TrimSpace(k.OwnerUserId)
	if owner == "" {
		return lifted{}, fmt.Errorf("procedure: refusing to lift a procedure with no owner -- the row would be readable by nobody")
	}
	actorCtx := ownerActor(ctx, owner)

	v, err := i.prepareVersion(actorCtx, k, mined)
	if err != nil {
		return lifted{}, err
	}
	existing, err := i.procedureByName(actorCtx, v.name)
	if err != nil {
		return lifted{}, err
	}
	switch {
	case existing == nil:
		return i.createProcedure(actorCtx, k, v)
	case storedRung(existing) == work.RungRetired && str(existing, "procedureHash") != v.hash:
		// Retirement is terminal. A changed procedure is a NEW candidate,
		// written as a new construct, never a resurrection of the retired
		// one -- and procedureConstructByName answers the newest, so the next
		// lift continues it.
		return i.createProcedure(actorCtx, k, v)
	case str(existing, "procedureHash") == v.hash:
		return i.keepProcedure(actorCtx, k, v, existing)
	default:
		return i.reliftProcedure(actorCtx, k, v, existing)
	}
}

// prepareVersion builds the procedure, renders it, runs Gate 1 and decides
// the entry rung. It reads (the goal statement, the Library rows the corpus
// named) and writes nothing.
func (i *Integration) prepareVersion(ctx context.Context, k corpusKey, mined *minedCorpus) (version, error) {
	win := mined.win
	t := win.Candidate.Template
	insts := occurrenceInstances(win.Candidate.Occurrences, mined.corpus)
	built, prec := i.buildProcedure(ctx, k, t, insts, mined.recs)

	v := version{name: procedureName(k), title: built.Title, uses: win.Utility.Uses}
	if len(built.RecordedFrom.RunIds) > 0 {
		v.firstRunId = built.RecordedFrom.RunIds[0]
	}
	prov := planner.TemplateProvenance{
		App:       built.RecordedFrom.App,
		Model:     built.RecordedFrom.Model,
		Effort:    built.RecordedFrom.Effort,
		SessionId: strings.Join(built.RecordedFrom.SessionIds, ", "),
		RunIds:    built.RecordedFrom.RunIds,
		Uses:      win.Utility.Uses,
		Net:       win.Utility.Net,
	}
	source, everyStepWritten := renderProcedureSource(v.name, built.Title, t, freeHoles(t), prov)
	v.source = source

	// Gate 1, before any write. A procedure whose source does not compile is
	// not re-runnable, so it keeps no signature and enters as a candidate:
	// the source is the artifact a promotion approval pins, and an approval
	// of source that does not compile approves nothing anybody could read.
	v.report, v.gateRan = i.compile(source, v.name)
	v.reRunnable = v.gateRan && v.report.OK && everyStepWritten && compiledAutomation(v.report, v.name)
	v.rung, v.reason = work.EntryRung(candidateEvidence(win, t, insts, mined.recs))
	if !v.reRunnable {
		v.rung, v.reason = work.RungCandidate, notRunnableReason(v.gateRan, v.report, everyStepWritten)
	}

	var err error
	if v.procedure, err = asObject(built); err != nil {
		return v, fmt.Errorf("procedure: encoding the procedure payload: %w", err)
	}
	if v.preconditions, err = asObject(prec); err != nil {
		return v, fmt.Errorf("procedure: encoding the preconditions: %w", err)
	}
	if v.hash, err = procedureHash(v.source, v.procedure, v.preconditions); err != nil {
		return v, fmt.Errorf("procedure: hashing the procedure: %w", err)
	}
	return v, nil
}

// createProcedure writes a first version: the bundle, the construct, the
// payload, Gate 1's verdict, the ladder's entry rung -- and the signature
// last.
func (i *Integration) createProcedure(ctx context.Context, k corpusKey, v version) (lifted, error) {
	bundleId := id.NewShortId()
	constructId := id.NewShortId()

	if err := i.store.writeInternal(ctx, "mutation "+call("createAuthoringBundle", map[string]any{
		"bundleId":    bundleId,
		"title":       fmt.Sprintf("Procedure learned from %d recorded run(s)", v.uses),
		"summary":     v.title,
		"sourceRunId": v.firstRunId,
	})); err != nil {
		return lifted{}, fmt.Errorf("create bundle: %w", err)
	}
	if err := i.store.writeInternal(ctx, "mutation "+call("createAuthoringConstruct", map[string]any{
		"constructId":     constructId,
		"bundleId":        bundleId,
		"kind":            "automation",
		"name":            v.name,
		"targetNamespace": "procedure",
		"source":          v.source,
		"origin":          "procedure/automations.memql",
	})); err != nil {
		return lifted{}, fmt.Errorf("create construct: %w", err)
	}
	if err := i.recordProcedure(ctx, constructId, v); err != nil {
		return lifted{}, err
	}
	if err := i.recordValidation(ctx, bundleId, v.report, v.gateRan, v.reRunnable); err != nil {
		return lifted{}, fmt.Errorf("record validation: %w", err)
	}
	if err := i.writeLadderEntry(ctx, constructId, v.rung, v.reason); err != nil {
		return lifted{}, err
	}
	if err := i.recordSignature(ctx, constructId, k, v); err != nil {
		return lifted{}, err
	}
	i.log().Info("procedure: lifted",
		"constructId", constructId, "bundleId", bundleId, "name", v.name,
		"uses", v.uses, "rung", string(v.rung), "reRunnable", v.reRunnable, "procedureHash", v.hash)
	return lifted{constructId: constructId, outcome: LiftCreated, hash: v.hash, rung: v.rung}, nil
}

// keepProcedure is the same version: nothing is written, with two exceptions
// that are both RECOVERIES rather than changes.
//
//   - A version on no rung, or still a candidate, re-enters with the evidence
//     as it stands now. The first is a lift interrupted before its ladder
//     write, which would otherwise sit off the ladder forever; the second is a
//     candidate held by a dislike that a later like has answered -- its
//     procedure is unchanged, the feedback is not, and a candidate has no
//     counters to lose.
//   - A re-runnable version whose signature was never written gets it: the
//     same interruption, one write later.
func (i *Integration) keepProcedure(ctx context.Context, k corpusKey, v version, existing map[string]any) (lifted, error) {
	constructId := str(existing, "id")
	out := lifted{constructId: constructId, outcome: LiftUnchanged, hash: v.hash, rung: storedRung(existing)}
	if (out.rung == work.RungNone || out.rung == work.RungCandidate) && v.rung != out.rung {
		if err := i.writeLadderEntry(ctx, constructId, v.rung, v.reason); err != nil {
			return out, err
		}
		out.rung = v.rung
	}
	if strings.TrimSpace(str(existing, "goalSignature")) == "" {
		if err := i.recordSignature(ctx, constructId, k, v); err != nil {
			return out, err
		}
	}
	return out, nil
}

// reliftProcedure writes a changed version in place: the ladder FIRST, so the
// new version is never readable on a rung it did not earn, then the payload,
// Gate 1's verdict on it, and the signature when it can run.
//
// An open promotion approval is left exactly as it is -- deciding it compares
// the approval's pinned hash with the construct's CURRENT one and refuses --
// but the construct stops pointing at it, or the ladder, which never proposes
// twice while a proposal is open, could never propose the new version at all.
func (i *Integration) reliftProcedure(ctx context.Context, k corpusKey, v version, existing map[string]any) (lifted, error) {
	constructId := str(existing, "id")
	if err := i.writeLadderEntry(ctx, constructId, v.rung, v.reason); err != nil {
		return lifted{}, err
	}
	if err := i.recordProcedure(ctx, constructId, v); err != nil {
		return lifted{}, err
	}
	if bundleId := str(existing, "bundleId"); bundleId != "" {
		if err := i.recordValidation(ctx, bundleId, v.report, v.gateRan, v.reRunnable); err != nil {
			return lifted{}, fmt.Errorf("record validation: %w", err)
		}
	}
	if strings.TrimSpace(str(existing, "goalSignature")) != k.GoalSignature {
		if err := i.recordSignature(ctx, constructId, k, v); err != nil {
			return lifted{}, err
		}
	}
	i.log().Info("procedure: re-lifted a changed procedure onto the entry rung",
		"constructId", constructId, "name", v.name, "rung", string(v.rung),
		"from", str(existing, "procedureHash"), "to", v.hash)
	return lifted{constructId: constructId, outcome: LiftRelifted, hash: v.hash, rung: v.rung}, nil
}

// recordProcedure writes the three fields that are one version -- the source,
// the payload, the preconditions -- and the hash that names them.
func (i *Integration) recordProcedure(ctx context.Context, constructId string, v version) error {
	if err := i.store.writeInternal(ctx, "mutation "+call("recordProcedure", map[string]any{
		"constructId":   constructId,
		"source":        v.source,
		"procedure":     v.procedure,
		"preconditions": v.preconditions,
		"procedureHash": v.hash,
	})); err != nil {
		return fmt.Errorf("record procedure: %w", err)
	}
	return nil
}

// writeLadderEntry puts a version on its entry rung with a CLEAN slate: every
// counter zero, the binding evidence an explicit empty object (a cleared
// streak must overwrite the stored one, and an omitted field is left alone),
// and no open approval.
func (i *Integration) writeLadderEntry(ctx context.Context, constructId string, rung work.Rung, reason string) error {
	if err := i.store.writeInternal(ctx, "mutation "+call("recordConstructLadder", map[string]any{
		"constructId":         constructId,
		"ladder":              string(rung),
		"shadowMatches":       0,
		"canaryMatches":       0,
		"distinctBindings":    map[string]any{},
		"failures":            0,
		"insufficient":        0,
		"promotionApprovalId": "",
		"ladderReason":        reason,
		"ladderChangedAt":     i.clock().UTC().Format(timeLayout),
	})); err != nil {
		return fmt.Errorf("record ladder: %w", err)
	}
	return nil
}

// recordSignature writes the goal signature -- only for a version that can
// run. It is recordConstructGoalSignature's production caller, and it is what
// makes the procedure findable at all: procedureConstructsForGoalSignature
// reads it.
func (i *Integration) recordSignature(ctx context.Context, constructId string, k corpusKey, v version) error {
	if !v.reRunnable {
		i.log().Info("procedure: learned a procedure that is not re-runnable; goalSignature withheld",
			"constructId", constructId, "gateRan", v.gateRan, "reason", v.reason)
		return nil
	}
	if err := i.store.writeInternal(ctx, "mutation "+call("recordConstructGoalSignature", map[string]any{
		"constructId":   constructId,
		"goalSignature": k.GoalSignature,
	})); err != nil {
		return fmt.Errorf("record goal signature: %w", err)
	}
	return nil
}

// procedureByName is the owner's construct of this name, or nil
// (procedureConstructByName answers the most recently created one).
func (i *Integration) procedureByName(ctx context.Context, name string) (map[string]any, error) {
	rows, err := i.store.query(ctx, "query "+call("procedureConstructByName", map[string]any{"name": name}))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// storedRung reads a construct's rung. A value this build does not know is no
// rung: component/work never moves one, and neither does anything here.
func storedRung(row map[string]any) work.Rung {
	r, ok := work.ParseRung(str(row, "ladder"))
	if !ok {
		return work.RungNone
	}
	return r
}

// timeLayout is how every timestamp this package writes is spelled.
const timeLayout = "2006-01-02T15:04:05Z07:00"

func (i *Integration) compile(source, name string) (memql.SandboxReport, bool) {
	if i.compiler != nil {
		return i.compiler.CompileBundle([]memql.SandboxConstruct{{Kind: "automation", Name: name, Source: source}}), true
	}
	if ce, ok := i.store.engine.(compileEngine); ok {
		return ce.CompileBundle([]memql.SandboxConstruct{{Kind: "automation", Name: name, Source: source}}), true
	}
	return memql.SandboxReport{}, false
}

// compiledAutomation reports whether Gate 1 actually COMPILED the procedure.
// A binary with no automation compiler linked reports the automation kind as
// SKIPPED -- and a skipped construct does not fail the bundle, so the report
// reads OK having checked nothing. That is not a pass.
func compiledAutomation(report memql.SandboxReport, name string) bool {
	for _, d := range report.Diagnostics {
		if d.Kind == "automation" && d.Name == name {
			return d.OK && !d.Skipped
		}
	}
	return false
}

// notRunnableReason is the ladder's sentence for a version held at candidate
// because it cannot be run, naming which of the three reasons it is.
func notRunnableReason(gateRan bool, report memql.SandboxReport, everyStepWritten bool) string {
	switch {
	case !everyStepWritten:
		return "a step's arguments have no MemQL spelling, so its source cannot say what it does, and a procedure nobody can read is not replayed"
	case !gateRan:
		return "no compile gate is installed on the node that learned it, so its source was never checked"
	case !report.OK:
		return "its source did not pass the compile gate: " + firstFailure(report)
	default:
		return "the compile gate skipped it, so its source was never checked"
	}
}

func firstFailure(report memql.SandboxReport) string {
	for _, d := range report.Diagnostics {
		if !d.OK && !d.Skipped && d.Error != "" {
			return d.Error
		}
	}
	return "no diagnostic was reported"
}

func (i *Integration) recordValidation(ctx context.Context, bundleId string, report memql.SandboxReport, gateRan, reRunnable bool) error {
	status := "validated"
	failure := ""
	if gateRan && !report.OK {
		status = "failed"
		failure = "the learned procedure did not compile"
	}
	validation := map[string]any{
		"ok":         gateRan && report.OK,
		"gate1Ran":   gateRan,
		"reRunnable": reRunnable,
		"learnedBy":  "component/procedure",
	}
	var diagnostics []any
	for _, d := range report.Diagnostics {
		if d.OK || d.Skipped {
			continue
		}
		diagnostics = append(diagnostics, map[string]any{
			"constructName": d.Name,
			"severity":      "error",
			"message":       d.Error,
		})
	}
	if len(diagnostics) > 0 {
		validation["diagnostics"] = diagnostics
	}
	return i.store.writeInternal(ctx, "mutation "+call("recordBundleValidation", map[string]any{
		"bundleId":         bundleId,
		"status":           status,
		"failureReason":    failure,
		"validationReport": validation,
	}))
}

// procedureName is stable for one (owner, signature, level), so re-mining the
// same corpus proposes the same name rather than a new one every sweep -- and
// the name is what the idempotent lift finds the construct by.
func procedureName(k corpusKey) string {
	sig := k.GoalSignature
	if len(sig) > 12 {
		sig = sig[:12]
	}
	if sig == "" {
		sig = "unsigned"
	}
	return fmt.Sprintf("learnedProcedure_%s_l%d", sig, int(k.Level))
}

// freeHoles are the holes a caller supplies, in a stable order.
func freeHoles(t proc.Template) []proc.Hole {
	var out []proc.Hole
	for _, h := range t.Holes {
		if h.Class == proc.HoleFree {
			out = append(out, h)
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Id < out[b].Id })
	return out
}
