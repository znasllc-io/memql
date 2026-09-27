package procedure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/component/work"
	workspine "github.com/znasllc-io/memql/integrations/work"
)

// failingDeriver fails the test if it is ever asked. It is the instrument for
// D6's headline claim -- "induction spends no model" -- which is only
// checkable if something would notice a call.
type failingDeriver struct{ t *testing.T }

func (d failingDeriver) ProposeDerivation(context.Context, proc.Hole, [][]proc.Action) (string, error) {
	d.t.Fatal("a model was asked to explain a hole the rules already explained: " +
		"induction is supposed to spend no model")
	return "", nil
}

// recordingDeriver answers a fixed expression and counts the calls.
type recordingDeriver struct {
	expr  string
	calls int
}

func (d *recordingDeriver) ProposeDerivation(context.Context, proc.Hole, [][]proc.Action) (string, error) {
	d.calls++
	return d.expr, nil
}

func holeSet(classes ...proc.HoleClass) []proc.Hole {
	out := make([]proc.Hole, 0, len(classes))
	for i, c := range classes {
		out = append(out, proc.Hole{
			Id: string(rune('a' + i)), StepIndex: 1, Path: []string{string(rune('a' + i))},
			Type: "string", Class: c,
		})
	}
	return out
}

// TestExplainUnexplained_TheModelIsAskedOnlyForAnUnexplainedHole is the
// NEGATIVE CONTROL for D6. Every hole the rules explained must reach no
// provider at all.
func TestExplainUnexplained_TheModelIsAskedOnlyForAnUnexplainedHole(t *testing.T) {
	i := New(nil, nil)
	i.SetDeriver(failingDeriver{t: t})
	got := i.explainUnexplained(context.Background(),
		holeSet(proc.HoleDataFlow, proc.HoleConstant, proc.HoleFree), nil)
	if len(got) != 3 {
		t.Fatalf("got %d holes, want 3", len(got))
	}
}

func TestExplainUnexplained_WithNoDeriverAnUnexplainedHoleBecomesFree(t *testing.T) {
	// The ordinary path: no deriver installed at all, so nothing reaches a
	// provider and the procedure is still learned with one more parameter.
	i := New(nil, nil)
	got := i.explainUnexplained(context.Background(), holeSet(proc.HoleUnexplained), nil)
	if got[0].Class != proc.HoleFree {
		t.Fatalf("class = %q, want %q", got[0].Class, proc.HoleFree)
	}
}

func TestExplainUnexplained_AtMostOneCallPerTemplate(t *testing.T) {
	// D6 allows ONE bounded model call. Two unexplained holes must not become
	// two calls.
	d := &recordingDeriver{expr: ""}
	i := New(nil, nil)
	i.SetDeriver(d)
	i.explainUnexplained(context.Background(),
		holeSet(proc.HoleUnexplained, proc.HoleUnexplained), nil)
	if d.calls != 1 {
		t.Fatalf("the deriver was called %d times; D6 allows one bounded call", d.calls)
	}
}

func TestExplainUnexplained_AProposalThatDoesNotHoldLeavesTheHoleFree(t *testing.T) {
	instances := [][]proc.Action{
		{
			{Tool: "exec", Args: proc.Obj(map[string]*proc.Node{"c": proc.Lit("x")}), Seq: 0,
				ResultValue: map[string]any{"path": "/srv/alpha.txt"}},
			{Tool: "fs_write", Args: proc.Obj(map[string]*proc.Node{"a": proc.Lit("alpha.txt")}), Seq: 1},
		},
		{
			{Tool: "exec", Args: proc.Obj(map[string]*proc.Node{"c": proc.Lit("x")}), Seq: 0,
				ResultValue: map[string]any{"path": "/srv/beta.txt"}},
			{Tool: "fs_write", Args: proc.Obj(map[string]*proc.Node{"a": proc.Lit("unrelated")}), Seq: 1},
		},
	}
	d := &recordingDeriver{expr: `basename(ref(0, "path"))`}
	i := New(nil, nil)
	i.SetDeriver(d)
	got := i.explainUnexplained(context.Background(), holeSet(proc.HoleUnexplained), instances)
	if got[0].Class != proc.HoleFree {
		t.Fatalf("class = %q, want %q -- the proposal explains one instance of two", got[0].Class, proc.HoleFree)
	}
	if got[0].Derivation != "" {
		t.Fatalf("a rejected derivation must not be recorded: %q", got[0].Derivation)
	}
}

func TestExplainUnexplained_AProposalThatHoldsIsKeptAsDataFlow(t *testing.T) {
	mk := func(n string) []proc.Action {
		return []proc.Action{
			{Tool: "exec", Args: proc.Obj(map[string]*proc.Node{"c": proc.Lit("x")}), Seq: 0,
				ResultValue: map[string]any{"path": "/srv/" + n + ".txt"}},
			{Tool: "fs_write", Args: proc.Obj(map[string]*proc.Node{"a": proc.Lit(n + ".txt")}), Seq: 1},
		}
	}
	d := &recordingDeriver{expr: `basename(ref(0, "path"))`}
	i := New(nil, nil)
	i.SetDeriver(d)
	got := i.explainUnexplained(context.Background(),
		holeSet(proc.HoleUnexplained), [][]proc.Action{mk("alpha"), mk("beta")})
	if got[0].Class != proc.HoleDataFlow {
		t.Fatalf("class = %q, want %q", got[0].Class, proc.HoleDataFlow)
	}
	if got[0].Derivation != `basename(ref(0, "path"))` {
		t.Fatalf("the kept derivation must be recorded; got %q", got[0].Derivation)
	}
}

func TestExplainUnexplained_AProposalOutsideTheGrammarIsRejected(t *testing.T) {
	d := &recordingDeriver{expr: `exec("rm -rf /")`}
	i := New(nil, nil)
	i.SetDeriver(d)
	got := i.explainUnexplained(context.Background(), holeSet(proc.HoleUnexplained), nil)
	if got[0].Class != proc.HoleFree {
		t.Fatalf("class = %q, want %q -- the grammar is the safety property", got[0].Class, proc.HoleFree)
	}
}

func TestLevelArg_DefaultsToActionsAndAcceptsTwo(t *testing.T) {
	if got := levelArg(nil); got != LevelAction {
		t.Fatalf("default level = %v, want %v", got, LevelAction)
	}
	if got := levelArg(map[string]any{"level": float64(2)}); got != LevelAutomation {
		t.Fatalf("level 2 = %v, want %v", got, LevelAutomation)
	}
	if got := levelArg(map[string]any{"level": float64(9)}); got != LevelAction {
		t.Fatalf("an unknown level must fall back to actions rather than mining nothing; got %v", got)
	}
}

func TestLevelAccepts_SeparatesTheTwoCorpusLevels(t *testing.T) {
	if !levelAccepts(LevelAction, "exec") || levelAccepts(LevelAction, "automation") {
		t.Fatal("level 1 is the app-session actions")
	}
	if !levelAccepts(LevelAutomation, "automation") || levelAccepts(LevelAutomation, "exec") {
		t.Fatal("level 2 is the automation invocations")
	}
}

func TestMarkConsumed_SetsConsumedWhenALaterStepReferencesTheResult(t *testing.T) {
	steps := []proc.Step{
		{StepType: "fs_read", Seq: 0, ResultValue: map[string]any{"body": "token-123"}},
		{StepType: "exec", Seq: 1, Input: map[string]any{"command": "deploy --key token-123"}},
		{StepType: "fs_read", Seq: 2, ResultValue: map[string]any{"body": "never-used"}},
	}
	markConsumed(steps)
	if !steps[0].Consumed {
		t.Error("a read whose value a later argument carries is consumed")
	}
	if steps[2].Consumed {
		t.Error("a read nothing referenced is not consumed")
	}
}

func TestMarkConsumed_ADigestOnlyResultIsStillRecognised(t *testing.T) {
	// A large result degrades to a digest. A step whose result was too big to
	// keep must still be recognised as consumed when a later argument carries
	// that digest, or every big read looks like noise.
	steps := []proc.Step{
		{StepType: "fetch", Seq: 0, ResultDigest: "sha256:abc"},
		{StepType: "exec", Seq: 1, Input: map[string]any{"command": "verify sha256:abc"}},
	}
	markConsumed(steps)
	if !steps[0].Consumed {
		t.Error("a digest carried into a later argument is a reference")
	}
}

// --- gap G10: the automations can act ---------------------------------------

func maintenanceCtx(automation string) context.Context {
	return auth.ContextWithAccess(context.Background(), auth.MaintenanceActor(automation))
}

func personCtx(userId string) context.Context {
	return auth.ContextWithUserActor(context.Background(), userId)
}

// triggerCtx is the engine-owned completion trigger as the automation
// executor presents it: the automation's synthetic actor, with the internal
// origin a tree-loaded automation runs under.
func triggerCtx() context.Context {
	return auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(),
		&auth.AccessContext{UserId: "system:automation:learnFromSucceededRun", Synthetic: true}))
}

// TestTheCompletionTriggerLearnsAsTheRunsOwner is G10, closed the narrow way.
// The event carries the run's owner, and the trusted completion trigger -- and
// nothing else -- borrows exactly that owner: the run is read through the
// OWNED read under the borrowed actor, so a hint that does not own the run
// reads nothing, and every later call runs as that owner. No cluster-wide
// read is involved at all.
func TestTheCompletionTriggerLearnsAsTheRunsOwner(t *testing.T) {
	eng := newFakeEngine()
	recs := twoRecordings()
	seedCorpus(t, eng, recs...)
	eng.reply("workRunForOwner", recs[1].runRow())
	i := newTestIntegration(eng)
	i.SetCompiler(&passingGate{})

	nodes, err := i.handleLearnFromRun(triggerCtx(), map[string]any{"runId": recs[1].runId, "ownerUserId": testOwner}, 0)
	if err != nil {
		t.Fatalf("the completion trigger was refused: %v", err)
	}
	var reply map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &reply); err != nil {
		t.Fatal(err)
	}
	if reply["accepted"] != true || reply["lift"] != string(LiftCreated) || reply["rung"] != "shadow" {
		t.Fatalf("reply = %v, want a lift into shadow", reply)
	}
	if len(eng.callsTo("workRunById")) != 0 {
		t.Fatal("the trigger reached the cluster-owner by-id read; it borrows the owner instead")
	}
	for _, c := range eng.recorded() {
		if c.Actor != testOwner {
			t.Fatalf("%s ran as %q; every call is the borrowed owner's", c.Name(), c.Actor)
		}
	}
}

// TestTheMaintenancePrincipalCannotLearnFromARun: least privilege. Only the
// completion trigger may borrow an owner; a cluster-wide principal -- or any
// other system actor -- is refused before it reads anything.
func TestTheMaintenancePrincipalCannotLearnFromARun(t *testing.T) {
	eng := newFakeEngine()
	// A LISTED principal: learnFromSucceededRun is no longer on the
	// maintenance list, so MaintenanceActor would answer nil for it and the
	// test would be measuring "no actor" instead of "the wrong actor".
	_, err := newTestIntegration(eng).handleLearnFromRun(maintenanceCtx("demoteProcedures"),
		map[string]any{"runId": "v1:work:run:r1", "ownerUserId": testOwner}, 0)
	if err == nil || !strings.Contains(err.Error(), "only the trusted completion trigger") {
		t.Fatalf("err = %v, want the trusted-trigger refusal", err)
	}
	if len(eng.recorded()) != 0 {
		t.Fatalf("a refused caller reached the engine: %s", eng.summary())
	}
}

// TestLearnFromRunReadsAPersonsRunThroughTheOwnedRead: a person names a run
// and reads it as themselves -- the owned read, unstamped -- and learns from
// their own corpus.
func TestLearnFromRunReadsAPersonsRunThroughTheOwnedRead(t *testing.T) {
	eng := newFakeEngine()
	recs := twoRecordings()
	seedCorpus(t, eng, recs...)
	eng.reply("workRunForOwner", recs[1].runRow())
	i := newTestIntegration(eng)
	i.SetCompiler(&passingGate{})
	// The caller's token carries the bare id; the row stores the canonical
	// one. They are the same person.
	res, err := i.learnFromRun(personCtx("alice"), recs[1].runId, LevelAction)
	if err != nil || !res.Accepted {
		t.Fatalf("the owner was refused their own run: %+v, %v", res, err)
	}
	if len(eng.callsTo("workRunById")) != 0 {
		t.Fatal("a person must not reach the cluster-owner by-id read")
	}
	first := eng.recorded()[0]
	if first.Name() != "workRunForOwner" || first.Internal {
		t.Fatalf("a person's run is read through the owned read, unstamped; got %s internal=%v", first.Name(), first.Internal)
	}
}

// TestLearnFromRunRefusesAPersonActingOnAnotherOwnersRun: G10 widened the
// SYNTHETIC caller, never a person. A person who can read somebody else's run
// -- a cluster owner can -- still may not learn from it: the procedure would
// be written into the other person's catalog under their name.
func TestLearnFromRunRefusesAPersonActingOnAnotherOwnersRun(t *testing.T) {
	eng := newFakeEngine()
	recs := twoRecordings()
	seedCorpus(t, eng, recs...)
	eng.reply("workRunForOwner", recs[1].runRow()) // readable, and alice's
	i := newTestIntegration(eng)
	_, err := i.learnFromRun(personCtx("v1:identity:user:mallory"), recs[1].runId, LevelAction)
	if err == nil || !strings.Contains(err.Error(), "belongs to another person") {
		t.Fatalf("err = %v, want the another-person refusal", err)
	}
	if len(eng.writes()) != 0 {
		t.Fatalf("a refused learn wrote %v", eng.summary())
	}

	// And a run the person cannot read at all is refused as not readable.
	empty := newFakeEngine()
	if _, err := newTestIntegration(empty).learnFromRun(personCtx("mallory"), recs[1].runId, LevelAction); err == nil ||
		!strings.Contains(err.Error(), "not readable") {
		t.Fatalf("err = %v, want the not-readable refusal", err)
	}
}

// TestHandleLearnFromRunAnswersThatAReplayRunIsNotARecording: the automation
// fires on EVERY run that succeeds, a procedure's own replays included, and
// this is the ordinary answer for one -- not an error, and never a lift.
func TestHandleLearnFromRunAnswersThatAReplayRunIsNotARecording(t *testing.T) {
	eng := newFakeEngine()
	replay := twoRecordings()[1]
	replay.triggeredBy = "procedure:canary"
	eng.reply("workRunForOwner", replay.runRow())
	nodes, err := newTestIntegration(eng).handleLearnFromRun(triggerCtx(),
		map[string]any{"runId": replay.runId, "ownerUserId": testOwner}, 0)
	if err != nil {
		t.Fatalf("a replay run must be answered, not refused: %v", err)
	}
	var reply map[string]any
	_ = json.Unmarshal(nodes[0].Payload, &reply)
	if reply["accepted"] != false || reply["reason"] != "a replay run is not a recording" {
		t.Fatalf("reply = %v", reply)
	}
	if len(eng.recorded()) != 1 {
		t.Fatalf("after reading the run, nothing else may happen: %s", eng.summary())
	}
}

// TestAnAutomationThatIsNotTheClusterReadsNoRun: a synthetic READER -- an
// automation that is not on the maintenance list -- is not the cluster. The
// by-id read is filtered to a cluster owner, so it reads nothing and the
// learn is refused as not readable.
func TestAnAutomationThatIsNotTheClusterReadsNoRun(t *testing.T) {
	eng := newFakeEngine() // workRunById answers nothing, as its filter would
	reader := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "system:automation:someProductAutomation", Role: auth.RoleReader, Unranked: true, Synthetic: true,
	})
	if _, err := newTestIntegration(eng).learnFromRun(reader, "v1:work:run:x", LevelAction); err == nil {
		t.Fatal("a synthetic reader learned from a run it cannot read")
	}
}

// TestMineCorpusIteratesOwnersForTheMaintenancePrincipal is G10's sweep. A
// cron firing carries no arguments, so the owner arrives blank -- and from the
// maintenance principal a blank owner means EVERY owner, each mined under
// that owner's own actor.
func TestMineCorpusIteratesOwnersForTheMaintenancePrincipal(t *testing.T) {
	eng := newFakeEngine()
	recs := twoRecordings()
	seedCorpus(t, eng, recs...)
	eng.reply("usersForSeedSweep", map[string]any{"id": testOwner}, map[string]any{"id": "v1:identity:user:bob"})
	eng.replyWhen("workRunsForOwner", "", recs[0].runRow(), recs[1].runRow())
	// The runs are alice's: bob's reads answer nothing, as the tier would.
	eng.ownedRead("workRunsForOwner", testOwner)
	eng.ownedRead("workRunsForOwnerGoalSignature", testOwner)
	i := newTestIntegration(eng)
	i.SetCompiler(&passingGate{})

	nodes, err := i.handleMineCorpus(maintenanceCtx("mineProcedureCorpusAcrossAutomations"), map[string]any{"ownerUserId": ""}, 0)
	if err != nil {
		t.Fatalf("the sweep was refused: %v", err)
	}
	var reply map[string]any
	_ = json.Unmarshal(nodes[0].Payload, &reply)
	if reply["owners"] != float64(2) || reply["accepted"] != true {
		t.Fatalf("reply = %v, want both owners walked and a lift", reply)
	}
	list := eng.callTo(t, "usersForSeedSweep")
	if !list.Internal || !list.Synthetic {
		t.Fatalf("the owner list must be the principal's stamped read; internal=%v synthetic=%v", list.Internal, list.Synthetic)
	}
	actors := map[string]bool{}
	for _, c := range eng.callsTo("workRunsForOwner") {
		actors[c.Actor] = true
	}
	if !actors[testOwner] || !actors["v1:identity:user:bob"] || len(actors) != 2 {
		t.Fatalf("each owner's goals must be read as that owner; read as %v", actors)
	}
	for _, c := range eng.writes() {
		if c.Actor != testOwner {
			t.Fatalf("%s was written as %q; a procedure is filed in its owner's catalog as the owner", c.Name(), c.Actor)
		}
	}
}

// TestMineCorpusRefusesABlankOwnerFromAPerson: only the cluster's principal
// turns a blank owner into "everybody". From a person it is refused, because
// a read with no actor answers zero rows and no error.
func TestMineCorpusRefusesABlankOwnerFromAPerson(t *testing.T) {
	eng := newFakeEngine()
	if _, err := newTestIntegration(eng).handleMineCorpus(personCtx(testOwner), map[string]any{"ownerUserId": ""}, 0); err == nil {
		t.Fatal("a person's blank owner was accepted")
	}
	if len(eng.recorded()) != 0 {
		t.Fatalf("a refused sweep reached the engine: %s", eng.summary())
	}
}

// TestMineCorpusRefusesAPersonNamingAnotherOwner: a person mines their OWN
// corpus. Naming somebody else is refused before anything is read.
func TestMineCorpusRefusesAPersonNamingAnotherOwner(t *testing.T) {
	eng := newFakeEngine()
	_, err := newTestIntegration(eng).handleMineCorpus(personCtx("mallory"),
		map[string]any{"ownerUserId": testOwner, "goalSignature": testSignature}, 0)
	if err == nil || !strings.Contains(err.Error(), "own corpus") {
		t.Fatalf("err = %v, want the own-corpus refusal", err)
	}
	if len(eng.recorded()) != 0 {
		t.Fatalf("a refused mine reached the engine: %s", eng.summary())
	}

	// The control: the same person naming THEMSELVES mines.
	own := newFakeEngine()
	seedCorpus(t, own, twoRecordings()...)
	i := newTestIntegration(own)
	i.SetCompiler(&passingGate{})
	if _, err := i.handleMineCorpus(personCtx("alice"),
		map[string]any{"ownerUserId": testOwner, "goalSignature": testSignature}, 0); err != nil {
		t.Fatalf("a person mining their own corpus was refused: %v", err)
	}
	if len(own.writes()) == 0 {
		t.Fatal("the control mined nothing, so the refusal above proves nothing")
	}
}

// TestNoCapabilitySaysItIsNotWiredYet: importing the plug-in makes the boot
// audit demand every executor dsl/procedure names, and the four the ladder
// adds were first registered as stubs that refused by name. A stub passes the
// audit with the certification ladder INERT, so every capability is driven
// here with arguments that reach its handler, and none may answer with the
// stub's words -- or with no handler at all.
func TestNoCapabilitySaysItIsNotWiredYet(t *testing.T) {
	caps := map[string]bool{}
	for _, c := range New(newFakeEngine(), nil).Capabilities() {
		caps[c.Name] = true
		if c.Handler == nil {
			t.Errorf("capability %s has no handler", c.Name)
			continue
		}
		_, err := c.Handler(maintenanceCtx("demoteProcedures"), map[string]any{
			"runId": "v1:work:run:x", "constructId": "v1:authoring:construct:x",
			"sweep": "demotion", "approvalId": "v1:work:approval:x", "ownerUserId": testOwner,
		}, 0)
		if err != nil && strings.Contains(err.Error(), "not wired yet") {
			t.Errorf("%s still answers as a stub: %v", c.Name, err)
		}
	}
	for _, name := range []string{"learnFromRun", "mineCorpus", "step", "replay", "ladderSweep", "decidePromotion"} {
		if !caps[name] {
			t.Errorf("capability %s is not registered; the boot audit refuses every node", name)
		}
	}
}

// TestTheReplayTargetsAgreeWithComponentWork pins the two spellings of a
// replay target together: component/procedure may not import component/work,
// so it repeats the words, and the lift writes work's spelling where the pure
// module compares its own.
func TestTheReplayTargetsAgreeWithComponentWork(t *testing.T) {
	if string(work.TargetWorkbench) != proc.TargetWorkbench || string(work.TargetMachine) != proc.TargetMachine {
		t.Fatalf("component/work targets %q/%q, component/procedure %q/%q",
			work.TargetWorkbench, work.TargetMachine, proc.TargetWorkbench, proc.TargetMachine)
	}
}

// TestTheReplayTriggerIsTheOneTheWorkSpineLeavesAlone pins the two spellings of
// a replay run's trigger together. This package WRITES it on every replay run,
// and integrations/work reads it to keep the run away from the template
// executor -- which would otherwise fail the live replay automation_not_runnable
// (epic memql#5408). integrations/work cannot import this package, so each
// spells the prefix, and a drift between them would bring that failure back
// with every test on both sides still green.
func TestTheReplayTriggerIsTheOneTheWorkSpineLeavesAlone(t *testing.T) {
	if replayTriggerPrefix != workspine.ProcedureReplayTriggerPrefix {
		t.Fatalf("replay runs are opened with %q, and integrations/work leaves runs triggered by %q to their runner",
			replayTriggerPrefix, workspine.ProcedureReplayTriggerPrefix)
	}
}

// runsOf is n succeeded runs of one goal signature, as workRunsForOwner
// answers them.
func runsOf(sig string, n int) []map[string]any {
	out := make([]map[string]any, n)
	for k := range out {
		out[k] = map[string]any{
			"id": fmt.Sprintf("v1:work:run:%s-%d", sig, k), "ownerUserId": testOwner,
			"goalSignature": sig, "status": "succeeded",
		}
	}
	return out
}

// TestTheCorpusSweepMinesAGoalOlderThanTheNewestPageOfRuns: the signatures a
// blank-signature mine walks were read from ONE page of workRunsForOwner -- an
// owner's newest runs, as many as the engine's default window holds -- so a
// goal last recorded further back than that was never mined again. The fake
// pages as the engine does: a full page answers a cursor, and the next read
// carries it. The replay run and the failed run on the last page are the
// filters' control: reading more pages must not widen what counts as a
// recording.
func TestTheCorpusSweepMinesAGoalOlderThanTheNewestPageOfRuns(t *testing.T) {
	replay := runsOf("sig-replay", 1)[0]
	replay["triggeredBy"] = "procedure:canary"
	failed := runsOf("sig-failed", 1)[0]
	failed["status"] = "failed"
	pages := map[string]struct {
		rows []map[string]any
		next string
	}{
		"":         {runsOf("sig-new", 3), "cursor-2"},
		"cursor-2": {runsOf("sig-mid", 3), "cursor-3"},
		"cursor-3": {append(runsOf("sig-old", 2), replay, failed), ""},
	}
	eng := newFakeEngine()
	var cursors []string
	eng.answer("workRunsForOwner", func(_ recordedCall, cursor string) ([]map[string]any, string) {
		cursors = append(cursors, cursor)
		return pages[cursor].rows, pages[cursor].next
	})
	// Read as anybody but the owner, the runs answer nothing, as the tier would.
	eng.ownedRead("workRunsForOwner", testOwner)

	if _, err := newTestIntegration(eng).handleMineCorpus(personCtx("alice"), map[string]any{"ownerUserId": testOwner}, 0); err != nil {
		t.Fatalf("mineCorpus: %v", err)
	}
	if got := strings.Join(cursors, ","); got != ",cursor-2,cursor-3" {
		t.Fatalf("workRunsForOwner was read from the cursors [%s], want every page in turn", got)
	}
	mined := map[string]bool{}
	for _, c := range eng.callsTo("workRunsForOwnerGoalSignature") {
		mined[parseCallArgs(t, c.Query)["goalSignature"].(string)] = true
	}
	for _, sig := range []string{"sig-new", "sig-mid", "sig-old"} {
		if !mined[sig] {
			t.Errorf("%s was not mined (mined %v): a goal past the newest page of runs is never learned from again", sig, mined)
		}
	}
	for _, sig := range []string{"sig-replay", "sig-failed"} {
		if mined[sig] {
			t.Errorf("%s was mined: a replay run and a failed run are not recordings", sig)
		}
	}
}

// TestTheSignatureReadStopsAtItsBoundAndSaysSo: a history that never runs out
// of pages -- larger than the bound, or a cursor that never ends -- is read up
// to maxSignatureRuns and no further, the signatures read so far are still
// mined, and the stop is LOGGED: a read that ended at a limit must not look
// like a history that ended there.
func TestTheSignatureReadStopsAtItsBoundAndSaysSo(t *testing.T) {
	const pageSize = 500
	eng := newFakeEngine()
	pagesRead := 0
	eng.answer("workRunsForOwner", func(_ recordedCall, _ string) ([]map[string]any, string) {
		pagesRead++
		return runsOf(fmt.Sprintf("sig-%03d", pagesRead), pageSize), fmt.Sprintf("cursor-%d", pagesRead+1)
	})
	var logs bytes.Buffer
	i := New(eng, slog.New(slog.NewTextHandler(&logs, nil)))

	sigs, err := i.ownerSignatures(context.Background(), testOwner)
	if err != nil {
		t.Fatalf("ownerSignatures: %v", err)
	}
	wantPages := (maxSignatureRuns + pageSize - 1) / pageSize
	if pagesRead != wantPages || len(sigs) != wantPages {
		t.Fatalf("read %d pages and found %d signatures, want %d of each: the read stops at the first page that reaches maxSignatureRuns (%d)",
			pagesRead, len(sigs), wantPages, maxSignatureRuns)
	}
	if !strings.Contains(logs.String(), "bound") || !strings.Contains(logs.String(), testOwner) {
		t.Errorf("the read stopped at its bound without saying so for whom: %q", logs.String())
	}
}
