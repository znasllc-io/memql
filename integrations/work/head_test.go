package work

import (
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/znasllc-io/memql/component/work"
)

// reassertFields is every per-version field reassertWorkStepVersion accepts
// (dsl/work/mutations.memql). A head move names ALL of them on every
// re-assertion: the update is a read-merge, and one left out would keep the
// later version's value under the earlier version's number.
var reassertFields = []string{
	"status", "result", "resultFingerprint", "binding", "postcondition", "symptom", "attempt", "version",
	"basis", "override", "authoredBy", "childRunId", "idempotencyKey", "startedAt", "finishedAt",
	"durationMs", "tokens", "cost", "errorCode", "errorMessage",
}

// headMoveFixture: four steps, all at version 1; then a re-run of draft
// produced draft v2 and review v2, and its publish v2 FAILED; then the person
// moved the head back to draft v1. Every version is still stored.
func headMoveFixture(t *testing.T) (*Integration, *recordingEngine) {
	t.Helper()
	i, eng, store := newActsIntegration(t)
	addPristineRun(store, actRunId, "fetch", "draft", "review", "publish")
	addVersion(store, actRunId, "draft", 1, 2, "done", work.Head{"fetch": {Version: 1}}, map[string]any{
		"override":   map[string]any{"level": "reasoning", "requestedBy": actOwner},
		"authoredBy": "",
		"binding":    map[string]any{"provider": "chat54", "level": "reasoning"},
	})
	addVersion(store, actRunId, "review", 2, 2, "done", work.Head{"fetch": {Version: 1}, "draft": {Version: 2}}, nil)
	addVersion(store, actRunId, "publish", 3, 2, "failed", work.Head{"fetch": {Version: 1}, "draft": {Version: 2}, "review": {Version: 2}}, map[string]any{
		"errorCode": "publish_refused", "errorMessage": "the channel refused the post",
	})
	run := actRunRow(runStatusFailed, "fetch", "draft", "review", "publish")
	run["head"] = work.Head{"fetch": {Version: 1}, "draft": {Version: 1}, "review": {Version: 1}, "publish": {Version: 1}}.Object()
	eng.reply("workRunForOwner", run)
	return i, eng
}

func TestAHeadMoveReassertsTheChosenVersionAndMarksOnlyTheUnmatchedStale(t *testing.T) {
	i, eng := headMoveFixture(t)

	nodes, err := i.handleMoveRunHead(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": "draft", "version": float64(2)}, 0)
	if err != nil {
		t.Fatalf("moveRunHead: %v", err)
	}

	// draft becomes v2 and review v2 is restored (it was computed from exactly
	// that upstream); publish v2 failed, so publish is the first step with no
	// finished version computed from the new upstream, and the only one stale.
	reasserts := eng.callsTo("reassertWorkStepVersion")
	if len(reasserts) != 2 {
		t.Fatalf("got %d re-assertions, want draft and review only (calls: %s)", len(reasserts), eng.summary())
	}
	byStep := map[string]map[string]any{}
	for _, c := range reasserts {
		if !c.Origin.IsInternal() || c.Actor != "v1:identity:user:"+actOwner {
			t.Errorf("a re-assertion ran as %q with origin %v; it is an @serverOnly write on the owner's row", c.Actor, c.Origin)
		}
		args := c.Args(t)
		byStep[args["stepId"].(string)] = args
	}
	draft := byStep["v1:work:step:r-acts-draft"]
	review := byStep["v1:work:step:r-acts-review"]
	if draft == nil || review == nil {
		t.Fatalf("re-asserted %v, want the draft and review rows", keysOfArgs(byStep))
	}
	for name, args := range map[string]map[string]any{"draft": draft, "review": review} {
		for _, field := range reassertFields {
			if _, named := args[field]; !named {
				t.Errorf("the %s re-assertion does not name %q; an update is a read-merge, so the later version's value would survive under this version's number", name, field)
			}
		}
		if args["version"] != float64(2) || args["status"] != "done" {
			t.Errorf("the %s re-assertion carries version %v status %v", name, args["version"], args["status"])
		}
	}
	if got := rowString(rowMap(draft, "result"), "result"); got != "draft version 2" {
		t.Errorf("draft re-asserted with result %q; the chosen version's row goes back verbatim", got)
	}
	if got := rowString(rowMap(draft, "override"), "level"); got != "reasoning" {
		t.Errorf("draft's own override did not go back with it: %v", draft["override"])
	}
	if basis := work.ParseHead(review["basis"]); !basis.Equal(work.Head{"fetch": {Version: 1}, "draft": {Version: 2}}) {
		t.Errorf("review's basis = %v", review["basis"])
	}

	update := argsOf(t, eng, "updateWorkRun")
	head := work.ParseHead(update["head"])
	wantHead := work.Head{"fetch": {Version: 1}, "draft": {Version: 2}, "review": {Version: 2}, "publish": {Version: 1}}
	if !head.Equal(wantHead) {
		t.Errorf("head = %v, want %v", head, wantHead)
	}
	if !reflect.DeepEqual(update["staleSteps"], []any{"publish"}) {
		t.Errorf("staleSteps = %v, want only publish", update["staleSteps"])
	}
	if update["status"] != runStatusRunning {
		t.Errorf("status = %v; a stale step means the run executes again", update["status"])
	}
	rerun, _ := update["rerun"].(map[string]any)
	if rerun["reason"] != rerunReasonHeadMove || rerun["stepKey"] != "publish" {
		t.Errorf("rerun = %v, want a headMove request from publish", rerun)
	}
	if o, _ := rerun["override"].(map[string]any); len(o) != 0 {
		t.Errorf("override = %v; after a head move the upstream is what changed, nobody's instructions", o)
	}
	// publish recorded v1 and a failed v2, so it runs again as v3 -- a number
	// taken from every version read, not from the newest row, which the
	// re-assertions have just made an EARLIER version.
	if !reflect.DeepEqual(rerun["versions"], map[string]any{"publish": float64(3)}) {
		t.Errorf("versions = %v, want publish at version 3", rerun["versions"])
	}
	if id, _ := rerun["requestId"].(string); id == "" {
		t.Error("the head move's request carries no id")
	}
	if update["heartbeatAt"] != rfc(testNow) {
		t.Errorf("heartbeatAt = %v; the write that flips the run to running stamps it", update["heartbeatAt"])
	}

	// The rows go first and the run last: the run's write is what can start
	// the re-run, and the executor it starts reads the collapsed rows.
	var order []string
	for _, c := range mutationsIn(eng) {
		order = append(order, c.Name())
	}
	if len(order) != 3 || order[2] != "updateWorkRun" {
		t.Errorf("writes in order %v; the run update must come after every re-assertion", order)
	}

	reply := decodeReply(t, nodes)
	if reply["stepKey"] != "draft" || reply["version"] != float64(2) || !reflect.DeepEqual(reply["staleSteps"], []any{"publish"}) {
		t.Errorf("reply = %v", reply)
	}
	assertEveryCallParses(t, eng)
}

// Review focus 3: a head moved to a version whose whole downstream matches
// re-runs NOTHING -- the energy and tokens spent once are not spent twice.
func TestAHeadMoveThatRestoresEverythingRerunsNothing(t *testing.T) {
	i, eng, store := newActsIntegration(t)
	addPristineRun(store, actRunId, "fetch", "draft", "review")
	addVersion(store, actRunId, "draft", 1, 2, "done", work.Head{"fetch": {Version: 1}}, nil)
	addVersion(store, actRunId, "review", 2, 2, "done", work.Head{"fetch": {Version: 1}, "draft": {Version: 2}}, nil)
	run := actRunRow(runStatusSucceeded, "fetch", "draft", "review")
	run["head"] = work.Head{"fetch": {Version: 1}, "draft": {Version: 2}, "review": {Version: 2}}.Object()
	eng.reply("workRunForOwner", run)

	nodes, err := i.handleMoveRunHead(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": "draft", "version": float64(1)}, 0)
	if err != nil {
		t.Fatalf("moveRunHead: %v", err)
	}
	var reasserted []string
	for _, c := range eng.callsTo("reassertWorkStepVersion") {
		args := c.Args(t)
		if args["version"] != float64(1) {
			t.Errorf("re-asserted version %v of %v, want version 1", args["version"], args["stepId"])
		}
		reasserted = append(reasserted, args["stepId"].(string))
	}
	sort.Strings(reasserted)
	if !reflect.DeepEqual(reasserted, []string{"v1:work:step:r-acts-draft", "v1:work:step:r-acts-review"}) {
		t.Errorf("re-asserted %v, want draft and review back at version 1 (fetch never changed)", reasserted)
	}
	update := argsOf(t, eng, "updateWorkRun")
	if !work.ParseHead(update["head"]).Equal(work.Head{"fetch": {Version: 1}, "draft": {Version: 1}, "review": {Version: 1}}) {
		t.Errorf("head = %v", update["head"])
	}
	if !reflect.DeepEqual(update["staleSteps"], []any{}) {
		t.Errorf("staleSteps = %v, want [] -- written, so an old stale list is cleared", update["staleSteps"])
	}
	for _, field := range []string{"status", "rerun", "heartbeatAt"} {
		if v, has := update[field]; has {
			t.Errorf("the run update names %s = %v; a head move that restores everything runs nothing and leaves the run finished", field, v)
		}
	}
	if got := decodeReply(t, nodes)["staleSteps"]; !reflect.DeepEqual(got, []any{}) {
		t.Errorf("reply staleSteps = %v", got)
	}
}

func TestAHeadMoveRefusesWhatItCannotMakeCurrent(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		version int
		code    string
	}{
		{"a version the run never recorded", "draft", 9, codeVersionNotFound},
		{"a version that did not finish", "publish", 2, codeVersionNotDone},
		{"a nested step", "draft/a", 1, codeStepNested},
		{"no version at all", "draft", 0, codeVersionNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i, eng := headMoveFixture(t)
			_, err := i.handleMoveRunHead(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": tc.key, "version": float64(tc.version)}, 0)
			var refusal *ActRefusal
			if !errors.As(err, &refusal) || refusal.Code != tc.code {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
			if writes := mutationsIn(eng); len(writes) != 0 {
				t.Errorf("a refused head move wrote: %s", eng.summary())
			}
		})
	}
}

func TestAHeadMoveOfARunStillWorkingIsRefused(t *testing.T) {
	i, eng, store := newActsIntegration(t)
	addPristineRun(store, actRunId, "fetch", "draft", "publish")
	eng.reply("workRunForOwner", actRunRow(runStatusRunning))
	_, err := i.handleMoveRunHead(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": "draft", "version": float64(1)}, 0)
	var refusal *ActRefusal
	if !errors.As(err, &refusal) || refusal.Code != codeRunNotFinished {
		t.Fatalf("err = %v, want %s", err, codeRunNotFinished)
	}
}

func keysOfArgs(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
