package procedure

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/component/work"
)

// persist_test.go -- the lift: the payload a replay needs, the version that
// pins it, and the idempotent write that puts it on the ladder (gaps G8, G9,
// G15, epic memql#5408).

func liftFixture(t *testing.T, recs ...recFixture) (*fakeEngine, LearnResult) {
	t.Helper()
	eng := newFakeEngine()
	seedCorpus(t, eng, recs...)
	i := newTestIntegration(eng)
	i.SetCompiler(&passingGate{})
	return eng, mustLift(t, i)
}

// relift runs a second lift against a construct the engine already holds.
func relift(t *testing.T, existing map[string]any, gate CompileGate, recs ...recFixture) (*fakeEngine, LearnResult) {
	t.Helper()
	eng := newFakeEngine()
	seedCorpus(t, eng, recs...)
	eng.reply("procedureConstructByName", existing)
	i := newTestIntegration(eng)
	i.SetCompiler(gate)
	return eng, mustLift(t, i)
}

func writeNames(eng *fakeEngine) []string {
	var out []string
	for _, c := range eng.writes() {
		out = append(out, c.Name())
	}
	return out
}

// TestTheLiftWritesTheProcedurePayloadTheRunnerNeeds: every key of the
// contract (plan section 1.3) is present on the stored payload, and it reads
// back through DecodeProcedure into a template the pure module binds -- the
// runner executes THIS, not the source.
func TestTheLiftWritesTheProcedurePayloadTheRunnerNeeds(t *testing.T) {
	eng, _ := liftFixture(t, twoRecordings()...)
	args := argsOf(t, eng.callTo(t, "recordProcedure"))
	payload, ok := args["procedure"].(map[string]any)
	if !ok {
		t.Fatalf("procedure arrived as %T, want an object", args["procedure"])
	}
	for _, key := range []string{"v", "level", "title", "goalSignature", "inputKeys", "steps", "holes", "expect",
		"inputMap", "freeParameters", "footprint", "target", "symbols", "model", "recordedFrom"} {
		if _, present := payload[key]; !present {
			t.Errorf("the payload lacks %q", key)
		}
	}
	from, _ := payload["recordedFrom"].(map[string]any)
	for _, key := range []string{"app", "model", "effort", "sessionIds", "runIds"} {
		if _, present := from[key]; !present {
			t.Errorf("recordedFrom lacks %q: %v", key, from)
		}
	}
	for n, s := range payload["steps"].([]any) {
		step := s.(map[string]any)
		for _, key := range []string{"tool", "args", "symbol"} {
			if _, present := step[key]; !present {
				t.Errorf("step %d lacks %q", n, key)
			}
		}
	}
	for n, s := range payload["symbols"].([]any) {
		sym := s.(map[string]any)
		for _, key := range []string{"id", "tool", "template"} {
			if _, present := sym[key]; !present {
				t.Errorf("symbol %d lacks %q", n, key)
			}
		}
	}

	p, err := DecodeProcedure(payload)
	if err != nil {
		t.Fatalf("the stored payload does not decode: %v", err)
	}
	if p.Title != testStatement || p.GoalSignature != testSignature || p.Level != 1 {
		t.Errorf("title / signature / level = %q / %q / %d", p.Title, p.GoalSignature, p.Level)
	}
	if !reflect.DeepEqual(p.InputKeys, []string{"file"}) {
		t.Errorf("inputKeys = %v, want the goal's input names", p.InputKeys)
	}
	if len(p.Steps) != 2 || p.Steps[0].Tool != "exec" || p.Steps[1].Tool != "fs_write" {
		t.Fatalf("steps = %+v, want the exec and the write", p.Steps)
	}
	if !reflect.DeepEqual(p.FreeParameters, []string{"s0.command.7"}) || p.InputMap["s0.command.7"] != "file" {
		t.Fatalf("free parameters %v / inputMap %v: the file name is the goal's `file` input", p.FreeParameters, p.InputMap)
	}
	for _, h := range p.Holes {
		if h.Evidence != 0 {
			t.Errorf("hole %s stores Evidence %d; a count of instances is the corpus's size, not the procedure's", h.Id, h.Evidence)
		}
	}
	// The expectations: the exec agreed exactly on its exit and type; the
	// report's bytes are the step's contract.
	if exp := p.Expect[0]; !exp.Exact || exp.ExitCode == nil || *exp.ExitCode != 0 || exp.ResultType != "string" || !exp.NoError {
		t.Errorf("exec expectation = %+v", exp)
	}
	wantWrite := work.StepExpectation{NoError: true, Exact: true,
		Contents: []work.ContentDigest{{Op: "write", Path: "out/report.txt", Digest: helloDigest}}}
	if !reflect.DeepEqual(p.Expect[1], wantWrite) {
		t.Errorf("write expectation = %+v, want %+v", p.Expect[1], wantWrite)
	}
	if p.Target != string(work.TargetWorkbench) || !p.Footprint.Files || p.Footprint.Machine {
		t.Errorf("footprint %+v / target %q: a write inside the workspace is portable", p.Footprint, p.Target)
	}
	if p.Model == nil || len(p.Model.Children) != 2 || p.Model.Children[0].Symbol != "s0" || p.Symbols[1].Id != "s1" {
		t.Errorf("model %+v / symbols %+v: the model is the sequence of the steps' own symbols", p.Model, p.Symbols)
	}
	if p.RecordedFrom.App != "claude-code" || p.RecordedFrom.Model != "claude-sonnet-4-6" || p.RecordedFrom.Effort != "high" ||
		len(p.RecordedFrom.SessionIds) != 2 || len(p.RecordedFrom.RunIds) != 2 {
		t.Errorf("recordedFrom = %+v", p.RecordedFrom)
	}

	// The template it decodes to binds a THIRD recording of the same goal --
	// recorded in ANOTHER workspace, and read the way the loader reads every
	// recording: relative to its own (relativize.go).
	third := proc.Canonicalize([]proc.Step{
		{StepType: "exec", Input: relativizeArgs(map[string]any{"command": "mkdir -p out && echo hello > c.txt"}, "/w/other")},
		{StepType: "fs_write", Input: relativizeArgs(map[string]any{"file_path": "/w/other/out/report.txt", "content": "hello\n"}, "/w/other")},
	})
	bound, ok := proc.BindInstance(p.Template(), third)
	if !ok || bound["s0.command.7"] != "c.txt" {
		t.Fatalf("the stored template does not bind a third recording: %v, %v", bound, ok)
	}

	prec, err := DecodePreconditions(args["preconditions"])
	if err != nil {
		t.Fatalf("preconditions do not decode: %v", err)
	}
	if prec.Tools["mkdir"] == "" || prec.Tools["echo"] == "" || prec.Tools["git"] != "" || prec.EmptyWorkspace == nil {
		t.Errorf("preconditions = %+v: the tools the commands use, not the ones the fingerprint merely probed", prec)
	}
}

// TestTheProcedureHashCoversSourceProcedureAndPreconditions: the hash is the
// version a promotion approval pins, so a change to what a replay DOES --
// the source's statements, the payload, the preconditions -- is a different
// hash. And a change to the PROVENANCE is not: every recording that agrees
// with a procedure adds a run, a session and a use to it, and a hash over
// those would make every agreeing recording a new version that re-enters the
// ladder.
func TestTheProcedureHashCoversSourceProcedureAndPreconditions(t *testing.T) {
	eng, res := liftFixture(t, twoRecordings()...)
	args := argsOf(t, eng.callTo(t, "recordProcedure"))
	source := args["source"].(string)
	payload := args["procedure"].(map[string]any)
	prec := args["preconditions"].(map[string]any)

	base, err := procedureHash(source, payload, prec)
	if err != nil {
		t.Fatal(err)
	}
	if base != res.ProcedureHash || args["procedureHash"] != base {
		t.Fatalf("the stored hash %v is not the hash of the stored three (%s)", args["procedureHash"], base)
	}
	if !strings.HasPrefix(base, "sha256:") || len(base) != len("sha256:")+64 {
		t.Fatalf("hash %q is not sha256: and 64 hex digits", base)
	}

	changed := func(what string, h string) {
		t.Helper()
		if h == base {
			t.Errorf("changing the %s left the version hash unchanged", what)
		}
	}
	h, _ := procedureHash(strings.Replace(source, `tool: "exec"`, `tool: "fetch"`, 1), payload, prec)
	changed("source's statements", h)
	mutated := cloneObject(t, payload)
	mutated["target"] = "machine"
	h, _ = procedureHash(source, mutated, prec)
	changed("procedure", h)
	precChanged := cloneObject(t, prec)
	precChanged["emptyWorkspace"] = false
	h, _ = procedureHash(source, payload, precChanged)
	changed("preconditions", h)

	same := func(what string, h string) {
		t.Helper()
		if h != base {
			t.Errorf("changing the %s changed the version hash; provenance is not behaviour", what)
		}
	}
	h, _ = procedureHash("// Recorded from somewhere else entirely.\n"+source, payload, prec)
	same("source's provenance comment", h)
	withProvenance := cloneObject(t, payload)
	withProvenance["recordedFrom"] = map[string]any{"runIds": []any{"another"}}
	h, _ = procedureHash(source, withProvenance, prec)
	same("recordedFrom", h)
	reworded := cloneObject(t, payload)
	reworded["title"] = "write the greeting file, reworded"
	h, _ = procedureHash(strings.Replace(source, "Write the greeting file", "write the greeting file, reworded", 1), reworded, prec)
	same("goal statement the title and the description quote", h)
}

func cloneObject(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	out, err := asObject(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestARelearnOfAnUnchangedCorpusWritesNothing is G15. A re-mine of a corpus
// that taught the procedure nothing new must write NOTHING: not the payload,
// not the ladder -- whose counters are the evidence a shadow streak has earned.
// Every mining pass used to mint a fresh construct under the same name.
func TestARelearnOfAnUnchangedCorpusWritesNothing(t *testing.T) {
	first, _ := liftFixture(t, twoRecordings()...)
	stored := storedConstruct(t, first, "shadow")

	eng, res := relift(t, stored, &passingGate{}, twoRecordings()...)
	if w := writeNames(eng); len(w) != 0 {
		t.Fatalf("an unchanged corpus wrote %v", w)
	}
	if res.Lift != LiftUnchanged || res.ConstructId != stored["id"] || res.Rung != work.RungShadow {
		t.Fatalf("result = %+v, want the stored construct, unchanged, still in shadow", res)
	}
}

// TestAnAgreeingRecordingLeavesTheVersionUnchanged is what makes the ladder
// climbable at all: the shadow comparison runs on each recording AFTER the
// lift that recording triggers, over a corpus one recording larger. A
// recording that agrees with the procedure must leave its version -- and so
// its counters -- alone, or no streak could ever reach m.
func TestAnAgreeingRecordingLeavesTheVersionUnchanged(t *testing.T) {
	first, _ := liftFixture(t, twoRecordings()...)
	stored := storedConstruct(t, first, "shadow")

	grown := append(twoRecordings(), recording1("c.txt", testNow.Add(-10*time.Minute)))
	eng, res := relift(t, stored, &passingGate{}, grown...)
	if w := writeNames(eng); len(w) != 0 {
		t.Fatalf("a third, agreeing recording wrote %v (hash %s, stored %v)", w, res.ProcedureHash, stored["procedureHash"])
	}
	if res.Sequences != 3 || res.Lift != LiftUnchanged {
		t.Fatalf("result = %+v, want three recordings and an unchanged version", res)
	}
}

// TestAChangedProcedureIsANewCandidate (D15: "a later change of the construct
// is a new candidate"). A re-lift that learned something different writes the
// new version in place and puts it back on the entry rung with a CLEAN slate
// -- and the ladder write comes FIRST: the construct may be trusted, and a
// new version written before its rung came down would be served unreviewed.
// An open approval is left alone, but the construct stops pointing at it.
func TestAChangedProcedureIsANewCandidate(t *testing.T) {
	first, _ := liftFixture(t, twoRecordings()...)
	stored := storedConstruct(t, first, "trusted")
	stored["procedureHash"] = "sha256:the-version-a-person-approved"
	stored["promotionApprovalId"] = "v1:work:approval:open"

	eng, res := relift(t, stored, &passingGate{}, twoRecordings()...)
	if res.Lift != LiftRelifted || res.ConstructId != stored["id"] {
		t.Fatalf("result = %+v, want the stored construct re-lifted in place", res)
	}
	names := writeNames(eng)
	if len(names) < 2 || names[0] != "recordConstructLadder" || names[1] != "recordProcedure" {
		t.Fatalf("writes = %v, want the ladder first, then the payload", names)
	}
	if len(eng.callsTo("createAuthoringConstruct")) != 0 || len(eng.callsTo("createAuthoringBundle")) != 0 {
		t.Fatalf("a re-lift minted a new construct: %v", names)
	}
	ladder := argsOf(t, eng.callTo(t, "recordConstructLadder"))
	if ladder["ladder"] != "shadow" {
		t.Errorf("rung = %v, want the entry rung the evidence earns (shadow)", ladder["ladder"])
	}
	for _, key := range []string{"shadowMatches", "canaryMatches", "failures", "insufficient"} {
		if ladder[key] != float64(0) {
			t.Errorf("%s = %v, want a clean slate", key, ladder[key])
		}
	}
	if b, ok := ladder["distinctBindings"].(map[string]any); !ok || len(b) != 0 {
		t.Errorf("distinctBindings = %v, want an explicit empty object", ladder["distinctBindings"])
	}
	if ladder["promotionApprovalId"] != "" {
		t.Errorf("promotionApprovalId = %v, want it cleared so the new version can be proposed", ladder["promotionApprovalId"])
	}
	if len(eng.callsTo("decideApproval")) != 0 {
		t.Error("the open approval row must be left alone")
	}
	if got := argsOf(t, eng.callTo(t, "recordProcedure"))["procedureHash"]; got != res.ProcedureHash {
		t.Errorf("recordProcedure wrote %v, want the new version %s", got, res.ProcedureHash)
	}
	// The signature is already the right one; nothing rewrites it.
	if len(eng.callsTo("recordConstructGoalSignature")) != 0 {
		t.Error("a re-lift rewrote an unchanged goal signature")
	}
}

// TestALiftedProcedureEntersShadowWhenTheGatePasses: two uses, every hole
// classified, no step held by a dislike, and a source that compiles -- the
// candidate gate passes and the procedure enters shadow, with the gate's
// sentence as the ladder's reason. The signature is written LAST: it is what
// makes the procedure findable, and nothing may find it half-written.
func TestALiftedProcedureEntersShadowWhenTheGatePasses(t *testing.T) {
	eng, res := liftFixture(t, twoRecordings()...)
	if res.Lift != LiftCreated || res.Rung != work.RungShadow {
		t.Fatalf("result = %+v, want a new construct in shadow", res)
	}
	ladder := argsOf(t, eng.callTo(t, "recordConstructLadder"))
	if ladder["ladder"] != "shadow" || !strings.Contains(ladder["ladderReason"].(string), "no step held by a dislike") {
		t.Fatalf("ladder write = %v", ladder)
	}
	if ladder["ladderChangedAt"] != testNow.Format(timeLayout) {
		t.Errorf("ladderChangedAt = %v, want the lift's clock", ladder["ladderChangedAt"])
	}
	names := writeNames(eng)
	if names[len(names)-1] != "recordConstructGoalSignature" {
		t.Fatalf("writes = %v, want the goal signature last", names)
	}
	sig := argsOf(t, eng.callTo(t, "recordConstructGoalSignature"))
	if sig["goalSignature"] != testSignature {
		t.Errorf("goalSignature = %v", sig["goalSignature"])
	}
	construct := argsOf(t, eng.callTo(t, "createAuthoringConstruct"))
	if construct["targetNamespace"] != "procedure" || construct["name"] != procedureName(corpusKeyFor()) {
		t.Errorf("construct = %v", construct)
	}
}

// TestADislikedInstanceStepHoldsTheLiftAtCandidate (D23): a person disliked
// the write in one recording and has liked no version of it since. The
// procedure is still lifted -- it is still what the recordings did -- but it
// stays a candidate, with the reason naming the instance and the step.
func TestADislikedInstanceStepHoldsTheLiftAtCandidate(t *testing.T) {
	recs := twoRecordings()
	recs[1].stepFeedback = []stepFeedback{{version: 1, verdict: "disliked", at: testNow.Add(-time.Minute)}}
	eng, res := liftFixture(t, recs...)
	if res.Rung != work.RungCandidate {
		t.Fatalf("rung = %s, want candidate", res.Rung)
	}
	reason := argsOf(t, eng.callTo(t, "recordConstructLadder"))["ladderReason"].(string)
	if !strings.Contains(reason, "instance 2, step 2") {
		t.Fatalf("reason %q must name the instance and the step", reason)
	}

	// The control: a liked version of the same step since clears it.
	recs[1].stepFeedback = append(recs[1].stepFeedback, stepFeedback{version: 2, verdict: "liked", at: testNow})
	_, cleared := liftFixture(t, recs...)
	if cleared.Rung != work.RungShadow {
		t.Fatalf("a liked later version must clear the dislike; rung = %s", cleared.Rung)
	}
}

// TestACandidateWhoseDislikeWasAnsweredEntersShadowOnTheNextLift: the version
// is unchanged -- feedback is not the procedure -- but a candidate has no
// counters to lose, and a dislike that a later like answered is exactly what
// the candidate gate is waiting for. Without this a disliked-then-liked
// procedure would sit at candidate forever.
func TestACandidateWhoseDislikeWasAnsweredEntersShadowOnTheNextLift(t *testing.T) {
	first, _ := liftFixture(t, twoRecordings()...)
	stored := storedConstruct(t, first, "candidate")
	eng, res := relift(t, stored, &passingGate{}, twoRecordings()...)
	if res.Lift != LiftUnchanged || res.Rung != work.RungShadow {
		t.Fatalf("result = %+v, want the unchanged version moved to shadow", res)
	}
	if names := writeNames(eng); !reflect.DeepEqual(names, []string{"recordConstructLadder"}) {
		t.Fatalf("writes = %v, want only the ladder", names)
	}
}

// TestAProcedureThatDoesNotCompileIsACandidateWithNoSignature: the source is
// the artifact a promotion approval pins. One that does not pass Gate 1 is not
// re-runnable: it is recorded, it keeps NO goal signature -- so nothing can
// find it -- and it enters as a candidate saying why.
func TestAProcedureThatDoesNotCompileIsACandidateWithNoSignature(t *testing.T) {
	eng := newFakeEngine()
	seedCorpus(t, eng, twoRecordings()...)
	i := newTestIntegration(eng)
	i.SetCompiler(failingGate{})
	res := mustLift(t, i)
	if res.Rung != work.RungCandidate {
		t.Fatalf("rung = %s, want candidate", res.Rung)
	}
	if len(eng.callsTo("recordConstructGoalSignature")) != 0 {
		t.Fatal("a procedure that does not compile was given a goal signature")
	}
	reason := argsOf(t, eng.callTo(t, "recordConstructLadder"))["ladderReason"].(string)
	if !strings.Contains(reason, "compile gate") {
		t.Fatalf("reason %q must say the source did not compile", reason)
	}
	if got := argsOf(t, eng.callTo(t, "recordBundleValidation"))["status"]; got != "failed" {
		t.Errorf("bundle status = %v, want failed", got)
	}
}

// TestNoGateIsNotAPass: a node with no compile gate installed has checked
// nothing, and nothing it has not checked is re-runnable.
func TestNoGateIsNotAPass(t *testing.T) {
	eng := newFakeEngine()
	seedCorpus(t, eng, twoRecordings()...)
	res := mustLift(t, newTestIntegration(eng))
	if res.Rung != work.RungCandidate || len(eng.callsTo("recordConstructGoalSignature")) != 0 {
		t.Fatalf("with no gate: rung %s, signature writes %d", res.Rung, len(eng.callsTo("recordConstructGoalSignature")))
	}
}

// TestARetiredProcedureThatChangedIsANewConstruct: retirement is terminal. A
// changed procedure is a NEW candidate, never a resurrection of the retired
// construct.
func TestARetiredProcedureThatChangedIsANewConstruct(t *testing.T) {
	first, _ := liftFixture(t, twoRecordings()...)
	stored := storedConstruct(t, first, "retired")
	stored["procedureHash"] = "sha256:the-retired-version"
	eng, res := relift(t, stored, &passingGate{}, twoRecordings()...)
	if res.Lift != LiftCreated || res.ConstructId == stored["id"] {
		t.Fatalf("result = %+v, want a new construct", res)
	}
	if len(eng.callsTo("createAuthoringConstruct")) != 1 {
		t.Fatalf("writes = %v, want a new construct created", writeNames(eng))
	}

	// The same version, retired, stays retired: nothing is written.
	stored["procedureHash"] = argsOf(t, first.callTo(t, "recordProcedure"))["procedureHash"]
	same, kept := relift(t, stored, &passingGate{}, twoRecordings()...)
	if len(same.writes()) != 0 || kept.Rung != work.RungRetired {
		t.Fatalf("an unchanged retired procedure wrote %v, rung %s", writeNames(same), kept.Rung)
	}
}

// TestEveryLiftWriteBorrowsTheOwnerAndStampsInternalOrigin: the authoring
// mutations are @serverOnly and the construct is owned -- so every write needs
// internal origin (without it: one WARN, no row) AND the owner's actor
// (without it the row is owned by nobody). parse_test.go holds the stamp to
// the loaded registry; this holds the actor.
func TestEveryLiftWriteBorrowsTheOwnerAndStampsInternalOrigin(t *testing.T) {
	eng, _ := liftFixture(t, twoRecordings()...)
	writes := eng.writes()
	if len(writes) == 0 {
		t.Fatal("the lift wrote nothing, so this asserts nothing")
	}
	for _, c := range writes {
		if !c.Internal {
			t.Errorf("%s was written without internal origin", c.Name())
		}
		if c.Actor != testOwner || c.Synthetic {
			t.Errorf("%s was written as %q (synthetic %v), want the owner", c.Name(), c.Actor, c.Synthetic)
		}
	}
	for _, c := range eng.recorded() {
		if !c.IsWrite() && c.Internal {
			t.Errorf("the read %s was stamped; an owned read runs unstamped under the owner", c.Name())
		}
	}
}

// TestTheLiftReadsNothingAsTheCluster: a lift driven by the maintenance
// principal must borrow the OWNER for every read after the first -- the
// composite tier would answer the principal every owner's rows, and a
// procedure mined from two people's recordings is correct about neither.
func TestTheLiftReadsNothingAsTheCluster(t *testing.T) {
	eng, _ := liftFixture(t, twoRecordings()...)
	var reads []string
	for _, c := range eng.recorded() {
		if c.Actor != testOwner {
			reads = append(reads, c.Name()+" as "+c.Actor)
		}
	}
	sort.Strings(reads)
	if len(reads) != 0 {
		t.Fatalf("calls not made as the owner: %v", reads)
	}
}

// TestAVersionWithAReplayRiskStaysACandidateNamingIt (B5): the recordings
// ran their command through `bash -c "<script>"`, so the parameter the
// template learned is the SCRIPT -- a goal's input would choose the code that
// runs, however it is quoted. The version is lifted (it is what the
// recordings did) and held at candidate, the ladder's reason the first
// sentence of component/procedure.ReplayRisks, however cleanly the candidate
// gate and Gate 1 pass.
func TestAVersionWithAReplayRiskStaysACandidateNamingIt(t *testing.T) {
	recs := twoRecordings()
	for n := range recs {
		recs[n].execCommand = `bash -c "mkdir -p out && echo hello > ` + recs[n].file + `"`
	}
	eng, res := liftFixture(t, recs...)
	if res.Rung != work.RungCandidate {
		t.Fatalf("rung = %s, want candidate: a version whose parameter is a script never climbs", res.Rung)
	}
	reason := argsOf(t, eng.callTo(t, "recordConstructLadder"))["ladderReason"].(string)
	if !containsAll(reason, "step 0", "script bash runs") {
		t.Fatalf("reason %q must be ReplayRisks' sentence naming the step and the script", reason)
	}

	// The control: the same goal recorded without the wrapper enters shadow.
	if _, ok := liftFixture(t, twoRecordings()...); ok.Rung != work.RungShadow {
		t.Fatalf("the control lifted to %s, so the hold above proves nothing", ok.Rung)
	}
}

// TestAVersionWithAParameterNoGoalInputSuppliesStaysACandidate: the two
// recordings wrote a-copy.txt and b-copy.txt while their goals' input named
// a.txt and b.txt, so the file name is a free parameter no goal input
// supplies (LearnInputMap ties it to nothing). The app's own actions bind it
// in shadow, so the version would earn a promotion there -- and then every
// canary start would be refused as unbound and counted, demoting it and
// asking the person again. It stays a candidate, the reason naming the
// parameter.
func TestAVersionWithAParameterNoGoalInputSuppliesStaysACandidate(t *testing.T) {
	recs := twoRecordings()
	for n := range recs {
		recs[n].execCommand = "mkdir -p out && echo hello > " + strings.TrimSuffix(recs[n].file, ".txt") + "-copy.txt"
	}
	eng, res := liftFixture(t, recs...)
	payload, err := DecodeProcedure(argsOf(t, eng.callTo(t, "recordProcedure"))["procedure"])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(payload.FreeParameters, []string{"s0.command.7"}) || len(payload.InputMap) != 0 {
		t.Fatalf("fixture: free parameters %v / inputMap %v, want one parameter no input supplies", payload.FreeParameters, payload.InputMap)
	}
	if res.Rung != work.RungCandidate {
		t.Fatalf("rung = %s, want candidate: no canary or trusted replay could bind its parameter", res.Rung)
	}
	reason := argsOf(t, eng.callTo(t, "recordConstructLadder"))["ladderReason"].(string)
	if !containsAll(reason, "parameter s0.command.7", "no goal input supplies it") {
		t.Fatalf("reason %q must name the parameter no goal input supplies", reason)
	}

	// The control: the goal's input names the file the recordings wrote.
	if _, ok := liftFixture(t, twoRecordings()...); ok.Rung != work.RungShadow {
		t.Fatalf("the control lifted to %s, so the hold above proves nothing", ok.Rung)
	}
}

// TestACodexCorpusLiftsIntoShadowWithItsParameterInsideTheScript: the same
// goal recorded by Codex, every command the vector ["bash", "-lc",
// "<script>"]. The script is read as the command line it is, so the file
// name is a parameter INSIDE it -- tied to the goal's `file` input like any
// other -- rather than the whole script as a parameter no replay could be
// trusted with. The version enters shadow, and its template writes a new
// goal's value back into the script.
func TestACodexCorpusLiftsIntoShadowWithItsParameterInsideTheScript(t *testing.T) {
	recs := twoRecordings()
	for n := range recs {
		recs[n].execVector = []any{"bash", "-lc", "mkdir -p out && echo hello > " + recs[n].file}
	}
	eng, res := liftFixture(t, recs...)
	if res.Rung != work.RungShadow {
		reason := argsOf(t, eng.callTo(t, "recordConstructLadder"))["ladderReason"]
		t.Fatalf("rung = %s (%v), want shadow: the parameter is a word of the script, not the script", res.Rung, reason)
	}
	p, err := DecodeProcedure(argsOf(t, eng.callTo(t, "recordProcedure"))["procedure"])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.FreeParameters, []string{"s0.command.2.7"}) || p.InputMap["s0.command.2.7"] != "file" {
		t.Fatalf("free parameters %v / inputMap %v, want the file inside the script, supplied by the goal's `file`", p.FreeParameters, p.InputMap)
	}
	v, err := proc.Materialize(p.Steps[0].Args, map[string]string{"s0.command.2.7": "e.txt"})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if got, want := v.(map[string]any)["command"], []any{"bash", "-lc", "mkdir -p out && echo hello > e.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("command = %q, want %q", got, want)
	}
}
