package work

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/work"
)

func TestABranchCreatesAForkRunPointingThePrefixAtTheSource(t *testing.T) {
	i, eng, store := newActsIntegration(t)
	addPristineRun(store, actRunId, "fetch", "draft", "publish")
	eng.reply("workRunForOwner", actRunRow(runStatusSucceeded))

	nodes, err := i.handleBranchRun(callerContext(actOwner), map[string]any{
		"runId":   actRunId,
		"stepKey": "draft",
		"model":   "app:claude-code:opus",
		"effort":  "high",
	}, 0)
	if err != nil {
		t.Fatalf("branchRun: %v", err)
	}

	writes := mutationsIn(eng)
	if len(writes) != 1 || writes[0].Name() != "createWorkRun" {
		t.Fatalf("a branch is ONE write -- the create, carrying its head and its request -- and nothing on the source; got %s", eng.summary())
	}
	create := writes[0]
	if !create.Origin.IsInternal() || create.Actor != "v1:identity:user:"+actOwner {
		t.Errorf("the fork was created as %q with origin %v; it belongs to the SOURCE's owner and createWorkRun is @serverOnly", create.Actor, create.Origin)
	}
	args := create.Args(t)
	forkId, _ := args["runId"].(string)
	if forkId == "" || forkId == actRunId || !strings.HasPrefix(forkId, runConcept+":") {
		t.Fatalf("fork run id = %q", forkId)
	}
	for k, want := range map[string]any{
		"mode":                modeFork,
		"forkedFromRunId":     actRunId,
		"forkAtStepKey":       "draft",
		"triggeredBy":         "branch:r-acts",
		"status":              runStatusRunning,
		"goalId":              actGoalId,
		"goalSignature":       "sig-weekly-report",
		"automationName":      "weeklyReport",
		"templateFingerprint": "fp-weekly",
		"templateConstructId": "v1:authoring:construct:weekly",
		"templateVersion":     "3",
		"inputFingerprint":    "in-39",
	} {
		if args[k] != want {
			t.Errorf("fork %s = %v, want %v", k, args[k], want)
		}
	}
	if !reflect.DeepEqual(args["variables"], map[string]any{"week": "2026-39", "region": "emea"}) {
		t.Errorf("fork variables = %v; the source's goal input is inherited whole", args["variables"])
	}
	if _, has := args["ownerUserId"]; has {
		t.Error("createWorkRun was called with an ownerUserId argument; the field is @serverSet from the actor")
	}
	// THE PREFIX IS SERVED BY REFERENCE: every step before the branch point
	// points at the source's current version of it, in the source run.
	head := work.ParseHead(args["head"])
	if !head.Equal(work.Head{"fetch": {Version: 1, RunId: actRunId}}) {
		t.Errorf("fork head = %v, want fetch v1 in the source run and nothing at or after draft", head)
	}
	rerun, _ := args["rerun"].(map[string]any)
	if rerun["reason"] != rerunReasonBranch || rerun["stepKey"] != "draft" || rerun["workspace"] != bareRunId(forkId) {
		t.Errorf("fork request = %v; a branch runs from its branch step, in a workspace named for the fork", rerun)
	}
	override, _ := rerun["override"].(map[string]any)
	if override["model"] != "app:claude-code:opus" || override["effort"] != "high" || override["requestedBy"] != actOwner {
		t.Errorf("fork override = %v", override)
	}

	reply := decodeReply(t, nodes)
	if reply["runId"] != forkId || reply["forkedFromRunId"] != actRunId || reply["forkAtStepKey"] != "draft" {
		t.Errorf("reply = %v", reply)
	}
	assertEveryCallParses(t, eng)
}

// A branch refuses what a re-run refuses: a run still working, and a step
// that is not one of its own.
func TestABranchRefusesARunStillWorkingAndAStepItDoesNotHave(t *testing.T) {
	cases := []struct {
		name, status, key, code string
	}{
		{"still running", runStatusRunning, "draft", codeRunNotFinished},
		{"nested", runStatusSucceeded, "draft/a", codeStepNested},
		{"unknown", runStatusSucceeded, "nope", codeStepNotInRun},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i, eng, store := newActsIntegration(t)
			addPristineRun(store, actRunId, "fetch", "draft", "publish")
			eng.reply("workRunForOwner", actRunRow(tc.status))
			_, err := i.handleBranchRun(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": tc.key}, 0)
			var refusal *ActRefusal
			if !errors.As(err, &refusal) || refusal.Code != tc.code {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
			if writes := mutationsIn(eng); len(writes) != 0 {
				t.Errorf("a refused branch wrote: %s", eng.summary())
			}
		})
	}
}

// sessionFixture: plan and code are steps an app session answered, test is
// not. plan's recording wrote src/a.go, ran one command, and -- when omitted
// is set -- wrote src/big.bin above the per-file cap, so its content was never
// stored.
func sessionFixture(t *testing.T, omitted bool) (*Integration, *recordingEngine) {
	t.Helper()
	i, eng, store := newActsIntegration(t)
	addVersion(store, actRunId, "plan", 0, 1, "done", nil, map[string]any{"childRunId": "v1:work:run:rec-plan"})
	addVersion(store, actRunId, "code", 1, 1, "done", nil, map[string]any{"childRunId": "v1:work:run:rec-code"})
	addVersion(store, actRunId, "test", 2, 1, "done", nil, nil)
	eng.reply("workRunForOwner", actRunRow(runStatusSucceeded, "plan", "code", "test"))
	for _, rec := range []string{"rec-plan", "rec-code"} {
		eng.replyWhen("workRunForOwner", rec, map[string]any{
			"id": "v1:work:run:" + rec, "ownerUserId": "v1:identity:user:" + actOwner,
			"automationName": appSessionTemplate, "status": runStatusSucceeded,
		})
	}
	eng.replyWhen("workStepsForOwnerRun", "rec-plan", map[string]any{
		"key": "action-a1", "seq": float64(0), "fingerprint": map[string]any{"cwd": "/ws/r-acts"},
	})
	actions := []map[string]any{
		toolResult("o-1", 0, "Write", `{"file_path":"/ws/r-acts/src/a.go","content":"package a"}`, []any{"v1:library:file:f-a"}, nil),
		toolResult("o-2", 1, "Bash", `{"command":"go generate ./..."}`, nil, nil),
	}
	if omitted {
		actions = append(actions, toolResult("o-3", 2, "Write", `{"file_path":"/ws/r-acts/src/big.bin"}`, nil,
			[]any{"/ws/r-acts/src/big.bin: above the 1 MiB per-file cap (sha256 00ff)"}))
	}
	eng.replyWhen("workObservationsForOwnerRun", "rec-plan", actions...)
	return i, eng
}

// toolResult is one recorded action's evidence, as the session writer stores
// it (integrations/work/session.go actionData).
func toolResult(id string, seq int, tool, args string, refs, omitted []any) map[string]any {
	data := map[string]any{
		"tool": tool, "appActionId": "act-" + id, "sessionId": "v1:worker:appSession:s-1",
		"seq": float64(seq), "isError": false, "args": args,
	}
	if refs != nil {
		data["contentRefs"] = refs
	}
	if omitted != nil {
		data["contentOmitted"] = omitted
	}
	return map[string]any{
		"id": "v1:work:observation:" + id, "kind": observationKindToolResult,
		"stepKey": "action-" + id, "createdAt": rfc(testNow.Add(timeMinutes(seq))), "data": data,
	}
}

// Issue #5415's acceptance: a branch from a session step whose earlier
// recordings omitted a file's content is refused, naming the file -- a branch
// from a partial workspace would diverge from the run it claims to branch,
// and say nothing.
func TestABranchFromASessionStepWithAnOmittedFileIsRefusedNamingIt(t *testing.T) {
	i, eng := sessionFixture(t, true)

	_, err := i.handleBranchRun(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": "code"}, 0)
	var omitted *work.SnapshotOmittedError
	if !errors.As(err, &omitted) {
		t.Fatalf("err = %v, want a snapshot_content_omitted refusal", err)
	}
	if omitted.Path != "src/big.bin" || !strings.Contains(err.Error(), "src/big.bin") || !strings.Contains(err.Error(), "snapshot_content_omitted") {
		t.Errorf("the refusal %q does not name the file", err)
	}
	if writes := mutationsIn(eng); len(writes) != 0 {
		t.Errorf("a refused branch opened a run anyway: %s", eng.summary())
	}
}

func TestABranchOfASessionStepStartsFromTheRebuiltWorkspace(t *testing.T) {
	i, eng := sessionFixture(t, false)

	nodes, err := i.handleBranchRun(callerContext(actOwner), map[string]any{
		"runId": actRunId, "stepKey": "code", "prompt": "Write the tests first.",
	}, 0)
	if err != nil {
		t.Fatalf("branchRun: %v", err)
	}
	args := argsOf(t, eng, "createWorkRun")
	rerun, _ := args["rerun"].(map[string]any)
	snapshot := work.ParseSnapshot(rerun["snapshot"])
	if snapshot == nil {
		t.Fatalf("fork request = %v; a branch of a session step starts against the rebuilt workspace", rerun)
	}
	want := []work.SnapshotFile{{Path: "src/a.go", FileId: "v1:library:file:f-a"}}
	if !reflect.DeepEqual(snapshot.Files, want) {
		t.Errorf("snapshot files = %v, want %v (workspace-relative, from the recording's own arguments)", snapshot.Files, want)
	}
	// Review focus 5: the command is REPORTED, never silently restored.
	if snapshot.UnrecordedCommands != 1 {
		t.Errorf("unrecordedCommands = %d, want 1", snapshot.UnrecordedCommands)
	}
	if got := decodeReply(t, nodes)["unrecordedCommands"]; got != float64(1) {
		t.Errorf("reply unrecordedCommands = %v; the person hears about the command now", got)
	}
	forkId, _ := args["runId"].(string)
	if rerun["workspace"] != bareRunId(forkId) {
		t.Errorf("workspace = %v, want the fork's own id", rerun["workspace"])
	}
	if override, _ := rerun["override"].(map[string]any); override["prompt"] != "Write the tests first." {
		t.Errorf("override = %v", override)
	}
	// Every recording read runs under the owner's own actor, unstamped.
	for _, c := range eng.recorded() {
		if strings.HasPrefix(c.Query, "query ") && c.Origin.IsInternal() {
			t.Errorf("%s was stamped with internal origin; an owned read must stay the owner's", c.Construct())
		}
	}
}

// A re-run of a session step gets a fresh workspace of its own, named for the
// version it runs as, beside the snapshot.
func TestARerunOfASessionStepGetsAFreshWorkspaceNamedForItsVersion(t *testing.T) {
	i, eng := sessionFixture(t, false)

	if _, err := i.handleRerunStep(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": "code"}, 0); err != nil {
		t.Fatalf("rerunStep: %v", err)
	}
	rerun, _ := argsOf(t, eng, "updateWorkRun")["rerun"].(map[string]any)
	if rerun["workspace"] != "r-acts-v2" {
		t.Errorf("workspace = %v, want r-acts-v2", rerun["workspace"])
	}
	if snapshot := work.ParseSnapshot(rerun["snapshot"]); snapshot == nil || len(snapshot.Files) != 1 {
		t.Errorf("snapshot = %v", rerun["snapshot"])
	}
}

// A step whose version names a child run that is not a recording is not a
// session step, and neither is one whose version names none: there is nothing
// to rebuild, and it runs in the default workspace.
func TestAStepNoAppAnsweredCarriesNoSnapshot(t *testing.T) {
	i, eng, store := newActsIntegration(t)
	addVersion(store, actRunId, "plan", 0, 1, "done", nil, map[string]any{"childRunId": "v1:work:run:sub-1"})
	addVersion(store, actRunId, "code", 1, 1, "done", nil, map[string]any{"childRunId": "v1:work:run:sub-2"})
	eng.reply("workRunForOwner", actRunRow(runStatusSucceeded, "plan", "code"))
	eng.replyWhen("workRunForOwner", "sub-", map[string]any{"id": "v1:work:run:sub-2", "automationName": "summarize"})

	if _, err := i.handleRerunStep(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": "code"}, 0); err != nil {
		t.Fatalf("rerunStep: %v", err)
	}
	rerun, _ := argsOf(t, eng, "updateWorkRun")["rerun"].(map[string]any)
	if _, has := rerun["snapshot"]; has {
		t.Errorf("rerun = %v; a subrun that is not an app session leaves nothing to rebuild", rerun)
	}
	if _, has := rerun["workspace"]; has {
		t.Errorf("rerun = %v; only a session step moves to a fresh workspace", rerun)
	}
}
