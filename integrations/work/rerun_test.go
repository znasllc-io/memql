package work

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/work"
)

// Review focus 1: a re-run requested while the run is still running, waiting
// or compiling is refused rather than racing the live execution.
func TestARerunOfARunningRunIsRefused(t *testing.T) {
	for _, status := range []string{runStatusRunning, runStatusWaiting, runStatusCompiling} {
		t.Run(status, func(t *testing.T) {
			i, eng, store := newActsIntegration(t)
			addPristineRun(store, actRunId, "fetch", "draft", "publish")
			eng.reply("workRunForOwner", actRunRow(status))

			_, err := i.handleRerunStep(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": "draft"}, 0)
			var refusal *ActRefusal
			if !errors.As(err, &refusal) || refusal.Code != codeRunNotFinished {
				t.Fatalf("err = %v, want a %s refusal", err, codeRunNotFinished)
			}
			if !strings.Contains(err.Error(), status) {
				t.Errorf("the refusal %q does not say what the run is doing", err)
			}
			if writes := mutationsIn(eng); len(writes) != 0 {
				t.Errorf("a refused re-run wrote %d rows: %s", len(writes), eng.summary())
			}
		})
	}
}

// Review focus 2: a nested key, or one the run does not have, is refused
// naming the step.
func TestARerunOfANestedStepIsRefused(t *testing.T) {
	cases := []struct {
		key, code string
	}{
		{"decide/a", codeStepNested},
		{"for_x/0/touch", codeStepNested},
		{"nope", codeStepNotInRun},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			i, eng, store := newActsIntegration(t)
			addPristineRun(store, actRunId, "fetch", "draft", "publish")
			eng.reply("workRunForOwner", actRunRow(runStatusSucceeded, "fetch", "draft", "draft/a", "publish"))

			_, err := i.handleRerunStep(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": tc.key}, 0)
			var refusal *ActRefusal
			if !errors.As(err, &refusal) || refusal.Code != tc.code {
				t.Fatalf("err = %v, want a %s refusal", err, tc.code)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("the refusal %q does not name the step %q", err, tc.key)
			}
			if writes := mutationsIn(eng); len(writes) != 0 {
				t.Errorf("a refused re-run wrote %d rows: %s", len(writes), eng.summary())
			}
		})
	}
}

func TestARerunWritesTheRequestOnTheRunForTheAgent(t *testing.T) {
	i, eng, store := newActsIntegration(t)
	addPristineRun(store, actRunId, "fetch", "draft", "publish")
	run := actRunRow(runStatusCancelled)
	run["cancelRequested"] = true
	run["errorCode"] = "run_cancelled"
	eng.reply("workRunForOwner", run)

	nodes, err := i.handleRerunStep(callerContext(actOwner), map[string]any{
		"runId":   actRunId,
		"stepKey": "draft",
		"level":   "reasoning",
		"model":   "fleet:qwen3-72b",
		"prompt":  "Use the Q3 totals from the ledger.",
		"inputs":  map[string]any{"region": "apac"},
	}, 0)
	if err != nil {
		t.Fatalf("rerunStep: %v", err)
	}

	writes := mutationsIn(eng)
	if len(writes) != 1 || writes[0].Name() != "updateWorkRun" {
		t.Fatalf("a re-run must write exactly ONE run update and nothing else; got %s", eng.summary())
	}
	update := writes[0]
	if !update.Origin.IsInternal() {
		t.Errorf("the run update reached the engine with origin %v; updateWorkRun is @serverOnly and would be refused", update.Origin)
	}
	if update.Actor != "v1:identity:user:"+actOwner {
		t.Errorf("the run update ran as %q; an owned row is written as its owner, copied off the run row", update.Actor)
	}
	args := update.Args(t)
	if args["runId"] != actRunId || args["status"] != runStatusRunning {
		t.Errorf("run update = %v; the run goes back to running so its event reaches the agents", args)
	}
	// A run somebody once cancelled must not cancel its own re-run at the
	// first step boundary, and a finished run's end is no longer its state.
	if args["cancelRequested"] != false || args["errorCode"] != "" || args["finishedAt"] != "" {
		t.Errorf("the re-run carries the old run's end forward: cancelRequested=%v errorCode=%v finishedAt=%v", args["cancelRequested"], args["errorCode"], args["finishedAt"])
	}
	if args["heartbeatAt"] != rfc(testNow) {
		t.Errorf("heartbeatAt = %v, want the act's own time so the abandoned sweep measures silence from the re-run", args["heartbeatAt"])
	}
	if got := args["staleSteps"]; !reflect.DeepEqual(got, []any{"draft", "publish"}) {
		t.Errorf("staleSteps = %v, want the step and every step after it", got)
	}
	rerun, _ := args["rerun"].(map[string]any)
	if rerun["reason"] != rerunReasonRerun || rerun["stepKey"] != "draft" || rerun["requestedBy"] != actOwner {
		t.Errorf("rerun = %v", rerun)
	}
	if id, _ := rerun["requestId"].(string); strings.TrimSpace(id) == "" {
		t.Error("the request carries no id; a request served once must be distinguishable from the next")
	}
	if rerun["requestedAt"] != rfc(testNow) {
		t.Errorf("requestedAt = %v", rerun["requestedAt"])
	}
	// The plan's versions ride the request: the executor runs each stale step
	// as the version named here, never one it reads off the newest row.
	if got := rerun["versions"]; !reflect.DeepEqual(got, map[string]any{"draft": float64(2), "publish": float64(2)}) {
		t.Errorf("versions = %v, want draft and publish at version 2", got)
	}
	override, _ := rerun["override"].(map[string]any)
	want := map[string]any{
		"level":       "reasoning",
		"model":       "fleet:qwen3-72b",
		"prompt":      "Use the Q3 totals from the ledger.",
		"inputs":      map[string]any{"region": "apac"},
		"requestedBy": actOwner,
	}
	if !reflect.DeepEqual(override, want) {
		t.Errorf("override = %v, want %v", override, want)
	}
	if _, has := rerun["snapshot"]; has {
		t.Error("a step no app session answered carries no snapshot")
	}
	if _, has := rerun["workspace"]; has {
		t.Error("a step no app session answered keeps the default workspace")
	}

	reply := decodeReply(t, nodes)
	if reply["runId"] != actRunId || reply["stepKey"] != "draft" || reply["version"] != float64(2) {
		t.Errorf("reply = %v, want version 2 of draft", reply)
	}
	if !reflect.DeepEqual(reply["staleSteps"], []any{"draft", "publish"}) {
		t.Errorf("reply staleSteps = %v", reply["staleSteps"])
	}
	assertEveryCallParses(t, eng)
}

// The version a re-run opens is one past the HIGHEST recorded, even after a
// head move pointed the step back at an earlier one: a version number is
// never reused, and neither is the idempotency key a side effect ran under.
func TestARerunNeverReusesAVersionNumber(t *testing.T) {
	i, eng, store := newActsIntegration(t)
	addPristineRun(store, actRunId, "fetch", "draft", "publish")
	addVersion(store, actRunId, "draft", 1, 2, "done", work.Head{"fetch": {Version: 1}}, nil)
	addVersion(store, actRunId, "draft", 1, 3, "failed", work.Head{"fetch": {Version: 1}}, nil)
	run := actRunRow(runStatusSucceeded)
	run["head"] = work.Head{"fetch": {Version: 1}, "draft": {Version: 1}, "publish": {Version: 1}}.Object()
	eng.reply("workRunForOwner", run)

	nodes, err := i.handleRerunStep(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": "draft"}, 0)
	if err != nil {
		t.Fatalf("rerunStep: %v", err)
	}
	if got := decodeReply(t, nodes)["version"]; got != float64(4) {
		t.Errorf("version = %v, want 4 (one past the highest recorded, which is the failed 3)", got)
	}
	rerun, _ := argsOf(t, eng, "updateWorkRun")["rerun"].(map[string]any)
	if versions, _ := rerun["versions"].(map[string]any); versions["draft"] != float64(4) || versions["publish"] != float64(2) {
		t.Errorf("versions = %v, want draft 4 and publish 2", rerun["versions"])
	}
}

// Every request carries a FRESH id, head moves included: the agent claims each
// request once under its id, so a reused id is a request never served.
func TestEveryRequestCarriesAFreshId(t *testing.T) {
	i, eng := headMoveFixture(t)
	if _, err := i.handleRerunStep(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": "draft"}, 0); err != nil {
		t.Fatalf("first re-run: %v", err)
	}
	if _, err := i.handleRerunStep(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": "draft"}, 0); err != nil {
		t.Fatalf("second re-run: %v", err)
	}
	if _, err := i.handleMoveRunHead(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": "draft", "version": float64(2)}, 0); err != nil {
		t.Fatalf("head move: %v", err)
	}
	seen := map[string]bool{}
	for _, c := range eng.callsTo("updateWorkRun") {
		rerun, _ := c.Args(t)["rerun"].(map[string]any)
		id, _ := rerun["requestId"].(string)
		if strings.TrimSpace(id) == "" || seen[id] {
			t.Fatalf("request id %q is missing or reused among %v", id, seen)
		}
		seen[id] = true
	}
	if len(seen) != 3 {
		t.Errorf("got %d requests, want 3", len(seen))
	}
}

// Issue #5417's acceptance: a re-run after a dislike carries the reason as
// guidance, so the next attempt is told what was wrong instead of resampling.
func TestARerunAfterADislikeCarriesTheReasonInItsGuidance(t *testing.T) {
	dislike := feedbackRow("v1:work:observation:fb-1", "draft", 1, "dislike", work.Axes{Product: true, Process: true}, "the totals are missing", 1)

	t.Run("the dislike rides as guidance", func(t *testing.T) {
		i, eng, store := newActsIntegration(t)
		addPristineRun(store, actRunId, "fetch", "draft", "publish")
		eng.reply("workRunForOwner", actRunRow(runStatusSucceeded))
		eng.reply("workObservationsForOwnerRun", dislike)

		if _, err := i.handleRerunStep(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": "draft", "level": "reasoning"}, 0); err != nil {
			t.Fatalf("rerunStep: %v", err)
		}
		override := rerunOverride(t, eng)
		guidance, _ := override["guidance"].(map[string]any)
		if guidance == nil {
			t.Fatalf("override = %v; the dislike on the version being replaced did not ride the re-run", override)
		}
		if guidance["reason"] != "the totals are missing" || guidance["feedbackId"] != "v1:work:observation:fb-1" {
			t.Errorf("guidance = %v", guidance)
		}
		if axes, _ := guidance["axes"].(map[string]any); axes["product"] != true || axes["process"] != true || axes["performance"] != false {
			t.Errorf("guidance axes = %v", guidance["axes"])
		}
		if read := eng.callTo(t, "workObservationsForOwnerRun"); read.Origin.IsInternal() || read.Actor != "v1:identity:user:"+actOwner {
			t.Errorf("the feedback read ran as %q with origin %v; it is the owner's own read, unstamped", read.Actor, read.Origin)
		}
	})

	t.Run("a prompt that already says it carries no second copy", func(t *testing.T) {
		i, eng, store := newActsIntegration(t)
		addPristineRun(store, actRunId, "fetch", "draft", "publish")
		eng.reply("workRunForOwner", actRunRow(runStatusSucceeded))
		eng.reply("workObservationsForOwnerRun", dislike)

		if _, err := i.handleRerunStep(callerContext(actOwner), map[string]any{
			"runId": actRunId, "stepKey": "draft", "prompt": "The Totals Are Missing -- add them from the ledger.",
		}, 0); err != nil {
			t.Fatalf("rerunStep: %v", err)
		}
		if g, has := rerunOverride(t, eng)["guidance"]; has {
			t.Errorf("guidance = %v; the person's own prompt already carries the reason", g)
		}
	})

	t.Run("a like given since withdraws the dislike", func(t *testing.T) {
		i, eng, store := newActsIntegration(t)
		addPristineRun(store, actRunId, "fetch", "draft", "publish")
		eng.reply("workRunForOwner", actRunRow(runStatusSucceeded))
		eng.reply("workObservationsForOwnerRun", dislike,
			feedbackRow("v1:work:observation:fb-2", "draft", 1, "like", work.Axes{}, "", 2))

		if _, err := i.handleRerunStep(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": "draft"}, 0); err != nil {
			t.Fatalf("rerunStep: %v", err)
		}
		if g, has := rerunOverride(t, eng)["guidance"]; has {
			t.Errorf("guidance = %v; the person's newest verdict on the version is a like", g)
		}
	})

	t.Run("a dislike on another version stays with that version", func(t *testing.T) {
		i, eng, store := newActsIntegration(t)
		addPristineRun(store, actRunId, "fetch", "draft", "publish")
		addVersion(store, actRunId, "draft", 1, 2, "done", work.Head{"fetch": {Version: 1}}, nil)
		eng.reply("workRunForOwner", actRunRow(runStatusSucceeded))
		eng.reply("workObservationsForOwnerRun", dislike)

		if _, err := i.handleRerunStep(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": "draft"}, 0); err != nil {
			t.Fatalf("rerunStep: %v", err)
		}
		if g, has := rerunOverride(t, eng)["guidance"]; has {
			t.Errorf("guidance = %v; the head is draft v2 and the dislike named v1", g)
		}
	})
}

// An override the engine could not honour is refused before anything is read
// or written.
func TestARerunRefusesAnOverrideItCannotHonour(t *testing.T) {
	cases := map[string]map[string]any{
		work.OverrideLevelInvalid:  {"level": "embeddings"},
		work.OverrideEffortInvalid: {"effort": "extreme"},
		work.OverrideModelInvalid:  {"model": "two words"},
	}
	for code, override := range cases {
		t.Run(code, func(t *testing.T) {
			i, eng, _ := newActsIntegration(t)
			args := map[string]any{"runId": actRunId, "stepKey": "draft"}
			for k, v := range override {
				args[k] = v
			}
			_, err := i.handleRerunStep(callerContext(actOwner), args, 0)
			var refusal *work.OverrideError
			if !errors.As(err, &refusal) || refusal.Code != code {
				t.Fatalf("err = %v, want %s", err, code)
			}
			if got := eng.summary(); got != "(none)" {
				t.Errorf("the engine was reached before the refusal: %s", got)
			}
		})
	}
}

// feedbackRow is one feedback observation in the shape recordFeedback writes.
func feedbackRow(id, stepKey string, version int, verdict string, axes work.Axes, reason string, minute int) map[string]any {
	target := map[string]any{}
	if stepKey != "" {
		target = map[string]any{"stepKey": stepKey, "version": float64(version)}
	}
	return map[string]any{
		"id":        id,
		"runId":     actRunId,
		"stepKey":   stepKey,
		"kind":      "feedback",
		"createdAt": rfc(testNow.Add(timeMinutes(minute))),
		"data": map[string]any{
			"verdict": verdict,
			"axes":    axes.Object(),
			"reason":  reason,
			"target":  target,
		},
	}
}

// rerunOverride is the override on the one run update the handler wrote.
func rerunOverride(t *testing.T, eng *recordingEngine) map[string]any {
	t.Helper()
	rerun, _ := argsOf(t, eng, "updateWorkRun")["rerun"].(map[string]any)
	override, _ := rerun["override"].(map[string]any)
	return override
}

// A run the executor cannot run again is refused by every act that would ask
// it to: an app session's recording (dispatched, it would fail on a template
// nobody registered and leave the corpus), a run that never compiled, and a
// run with no goal.
func TestAnActOnARunTheExecutorCannotRunIsRefused(t *testing.T) {
	cases := map[string]func(map[string]any){
		"a recording": func(r map[string]any) {
			r["automationName"] = appSessionTemplate
			r["parentRunId"] = "v1:work:run:r-parent"
		},
		"a run that never compiled": func(r map[string]any) { r["automationName"] = compilingAutomationName },
		"a run with no goal":        func(r map[string]any) { r["goalId"] = "" },
	}
	acts := map[string]func(*Integration) error{
		"rerunStep": func(i *Integration) error {
			_, err := i.handleRerunStep(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": "draft"}, 0)
			return err
		},
		"branchRun": func(i *Integration) error {
			_, err := i.handleBranchRun(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": "draft"}, 0)
			return err
		},
		"moveRunHead": func(i *Integration) error {
			_, err := i.handleMoveRunHead(callerContext(actOwner), map[string]any{"runId": actRunId, "stepKey": "draft", "version": float64(1)}, 0)
			return err
		},
	}
	for name, mutate := range cases {
		for act, call := range acts {
			t.Run(name+"/"+act, func(t *testing.T) {
				i, eng, store := newActsIntegration(t)
				addPristineRun(store, actRunId, "fetch", "draft", "publish")
				run := actRunRow(runStatusSucceeded)
				mutate(run)
				eng.reply("workRunForOwner", run)
				err := call(i)
				var refusal *ActRefusal
				if !errors.As(err, &refusal) || refusal.Code != codeRunNotExecutable {
					t.Fatalf("err = %v, want %s", err, codeRunNotExecutable)
				}
				if writes := mutationsIn(eng); len(writes) != 0 {
					t.Errorf("a refused act wrote: %s", eng.summary())
				}
			})
		}
	}
}
