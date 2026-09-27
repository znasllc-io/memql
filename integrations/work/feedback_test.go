package work

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/work"
)

// feedbackFixture: a finished run whose draft has two versions, v2 current.
func feedbackFixture(t *testing.T) (*Integration, *recordingEngine) {
	t.Helper()
	i, eng, store := newActsIntegration(t)
	addPristineRun(store, actRunId, "fetch", "draft", "publish")
	addVersion(store, actRunId, "draft", 1, 2, "done", work.Head{"fetch": {Version: 1}}, nil)
	eng.reply("workRunForOwner", actRunRow(runStatusSucceeded))
	return i, eng
}

// Issue #5416's acceptance: a dislike saved without an axis is refused BY THE
// ACT -- the question is the point, and nothing is read or written first.
func TestADislikeWithoutAnAxisIsRefusedByTheAct(t *testing.T) {
	i, eng := feedbackFixture(t)
	_, err := i.handleRecordFeedback(callerContext(actOwner), map[string]any{
		"runId": actRunId, "stepKey": "draft", "verdict": "dislike", "reason": "not good",
	}, 0)
	if !errors.Is(err, work.ErrFeedbackAxisRequired) || !strings.Contains(err.Error(), "feedback_axis_required") {
		t.Fatalf("err = %v, want feedback_axis_required", err)
	}
	if got := eng.summary(); got != "(none)" {
		t.Errorf("the refused dislike reached the engine: %s", got)
	}

	// The same dislike naming an axis is accepted.
	if _, err := i.handleRecordFeedback(callerContext(actOwner), map[string]any{
		"runId": actRunId, "stepKey": "draft", "verdict": "dislike", "process": true, "reason": "not good",
	}, 0); err != nil {
		t.Fatalf("a dislike naming the process axis was refused: %v", err)
	}
}

// Issue #5416's acceptance: a later verdict is a NEW row and never rewrites an
// earlier one.
func TestALaterVerdictIsANewRow(t *testing.T) {
	i, eng := feedbackFixture(t)
	first, err := i.handleRecordFeedback(callerContext(actOwner), map[string]any{
		"runId": actRunId, "stepKey": "draft", "verdict": "dislike", "product": true, "reason": "the totals are missing",
	}, 0)
	if err != nil {
		t.Fatalf("first verdict: %v", err)
	}
	second, err := i.handleRecordFeedback(callerContext(actOwner), map[string]any{
		"runId": actRunId, "stepKey": "draft", "verdict": "like",
	}, 0)
	if err != nil {
		t.Fatalf("second verdict: %v", err)
	}

	writes := mutationsIn(eng)
	if len(writes) != 2 {
		t.Fatalf("two verdicts made %d writes: %s", len(writes), eng.summary())
	}
	ids := map[string]bool{}
	for _, w := range writes {
		if w.Name() != "createWorkObservation" {
			t.Errorf("a verdict wrote %s; it is an INSERT of a new observation, never an update", w.Name())
		}
		if !w.Origin.IsInternal() || w.Actor != "v1:identity:user:"+actOwner {
			t.Errorf("the verdict was written as %q with origin %v", w.Actor, w.Origin)
		}
		args := w.Args(t)
		if args["kind"] != "feedback" {
			t.Errorf("kind = %v", args["kind"])
		}
		ids[args["observationId"].(string)] = true
	}
	if len(ids) != 2 {
		t.Errorf("both verdicts were written under one id %v; a later verdict must never overwrite an earlier one", ids)
	}
	if a, b := decodeReply(t, first)["observationId"], decodeReply(t, second)["observationId"]; a == b || a == "" {
		t.Errorf("replies name observations %v and %v", a, b)
	}
	assertEveryCallParses(t, eng)
}

// TestAFeedbackRowHasTheShapeTheCorpusReads round-trips the row the act
// writes through a copy of the procedure corpus's parse (integrations/procedure
// corpus.go readFeedback), because the two packages cannot import each other
// and the shape between them is the contract epic D and this epic agreed:
// data.verdict, data.target.stepKey (a string; absent is the RUN) and
// data.target.version (a number, which arrives as a float64).
func TestAFeedbackRowHasTheShapeTheCorpusReads(t *testing.T) {
	i, eng := feedbackFixture(t)
	if _, err := i.handleRecordFeedback(callerContext(actOwner), map[string]any{
		"runId": actRunId, "stepKey": "draft", "version": float64(1), "verdict": "dislike",
		"product": true, "performance": true, "reason": "  the tone is wrong  ",
	}, 0); err != nil {
		t.Fatalf("step verdict: %v", err)
	}
	if _, err := i.handleRecordFeedback(callerContext(actOwner), map[string]any{
		"runId": actRunId, "verdict": "dislike", "process": true,
	}, 0); err != nil {
		t.Fatalf("run verdict: %v", err)
	}

	var rows []map[string]any
	for n, c := range eng.callsTo("createWorkObservation") {
		args := c.Args(t)
		rows = append(rows, map[string]any{
			"id": args["observationId"], "kind": args["kind"], "stepKey": args["stepKey"],
			"data": args["data"], "content": args["content"],
			"createdAt": testNow.Add(time.Duration(n) * time.Second).Format(time.RFC3339Nano),
		})
	}
	fb := corpusReadFeedback(rows)
	if !fb.runDisliked {
		t.Error("the run-level dislike did not read as the RUN's verdict; a target with no stepKey is the run")
	}
	verdicts := fb.steps["draft"]
	if len(verdicts) != 1 || verdicts[0].Version != 1 || verdicts[0].Verdict != work.VerdictDislike {
		t.Fatalf("the corpus read the step verdicts as %+v, want one dislike on draft v1", verdicts)
	}

	data := rowMap(rows[0], "data")
	if axes := work.ParseAxes(data["axes"]); !axes.Product || axes.Process || !axes.Performance {
		t.Errorf("axes = %v", data["axes"])
	}
	if data["reason"] != "the tone is wrong" || data["goalSignature"] != "sig-weekly-report" {
		t.Errorf("data = %v; the reason and the goal's signature ride on the row", data)
	}
	if got := rowString(rows[0], "content"); got != "Disliked version 1 of draft (product, performance): the tone is wrong." {
		t.Errorf("content = %q", got)
	}
	if target := rowMap(rowMap(rows[1], "data"), "target"); len(target) != 0 {
		t.Errorf("the run verdict's target = %v, want {}", target)
	}
	if _, named := rows[1]["stepKey"]; named && rows[1]["stepKey"] != nil {
		t.Errorf("the run verdict names step %v", rows[1]["stepKey"])
	}
}

// corpusVerdict and corpusReadFeedback are a COPY of integrations/procedure's
// stepVerdict and readFeedback, kept to the reads that decide the contract.
// When corpus.go changes how it reads a feedback row, this copy changes with
// it, and the test above then says whether the writer still agrees.
type corpusVerdict struct {
	Version int
	Verdict work.Verdict
}

type corpusFeedback struct {
	runDisliked bool
	steps       map[string][]corpusVerdict
}

func corpusReadFeedback(observations []map[string]any) corpusFeedback {
	fb := corpusFeedback{steps: map[string][]corpusVerdict{}}
	for _, o := range observations {
		if rowString(o, "kind") != "feedback" {
			continue
		}
		data := rowMap(o, "data")
		verdict := work.ParseVerdict(rowString(data, "verdict"))
		target := rowMap(data, "target")
		key := rowString(target, "stepKey")
		if key == "" {
			if verdict == work.VerdictDislike {
				fb.runDisliked = true
			}
			continue
		}
		fb.steps[key] = append(fb.steps[key], corpusVerdict{Version: rowInt(target, "version"), Verdict: verdict})
	}
	return fb
}

// A step verdict with no version judges the step's CURRENT version.
func TestAStepVerdictDefaultsToTheCurrentVersion(t *testing.T) {
	i, eng := feedbackFixture(t)
	if _, err := i.handleRecordFeedback(callerContext(actOwner), map[string]any{
		"runId": actRunId, "stepKey": "draft", "verdict": "neutral",
	}, 0); err != nil {
		t.Fatalf("recordFeedback: %v", err)
	}
	target := rowMap(rowMap(argsOf(t, eng, "createWorkObservation"), "data"), "target")
	if target["stepKey"] != "draft" || target["version"] != float64(2) {
		t.Errorf("target = %v, want draft at its current version 2", target)
	}
}

func TestAVerdictOnAVersionTheRunNeverRecordedIsRefused(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"an unrecorded version": {"stepKey": "draft", "version": float64(7)},
		"an unknown step":       {"stepKey": "nope"},
		"a nested step":         {"stepKey": "draft/a"},
	} {
		t.Run(name, func(t *testing.T) {
			i, eng := feedbackFixture(t)
			call := map[string]any{"runId": actRunId, "verdict": "like"}
			for k, v := range args {
				call[k] = v
			}
			_, err := i.handleRecordFeedback(callerContext(actOwner), call, 0)
			var refusal *ActRefusal
			if !errors.As(err, &refusal) || refusal.Code != codeFeedbackTargetNotFound {
				t.Fatalf("err = %v, want %s", err, codeFeedbackTargetNotFound)
			}
			if writes := mutationsIn(eng); len(writes) != 0 {
				t.Errorf("a refused verdict was written: %s", eng.summary())
			}
		})
	}
}

// A verdict is the OWNER's: somebody else's run is not there to judge.
func TestAVerdictOnSomebodyElsesRunIsRefused(t *testing.T) {
	i, eng, _ := newActsIntegration(t)
	_, err := i.handleRecordFeedback(callerContext("u-mallory"), map[string]any{"runId": actRunId, "verdict": "like"}, 0)
	var refusal *ActRefusal
	if !errors.As(err, &refusal) || refusal.Code != codeRunNotFound {
		t.Fatalf("err = %v, want %s", err, codeRunNotFound)
	}
	if writes := mutationsIn(eng); len(writes) != 0 {
		t.Errorf("a verdict on somebody else's run was written: %s", eng.summary())
	}
}

// The validator's verdict is kept beside the person's, never resolved: a like
// on an answer it flagged, or a dislike on one it passed, is recorded as a
// disagreement naming its observation; neutral never disagrees.
func TestTheValidatorDisagreementIsKept(t *testing.T) {
	flagged := validatorDecisionRow("v1:work:observation:val-1", "draft", 2, work.Axes{Process: true}, "it skipped the ledger")
	passed := validatorDecisionRow("v1:work:observation:val-2", "draft", 2, work.Axes{}, "nothing wrong")
	cases := []struct {
		name      string
		decision  map[string]any
		args      map[string]any
		disagrees bool
	}{
		{"a like on a flagged answer", flagged, map[string]any{"verdict": "like"}, true},
		{"a dislike on a passed answer", passed, map[string]any{"verdict": "dislike", "product": true}, true},
		{"a dislike on a flagged answer", flagged, map[string]any{"verdict": "dislike", "process": true}, false},
		{"neutral on a flagged answer", flagged, map[string]any{"verdict": "neutral"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i, eng := feedbackFixture(t)
			eng.reply("workObservationsForOwnerRun", tc.decision)
			call := map[string]any{"runId": actRunId, "stepKey": "draft"}
			for k, v := range tc.args {
				call[k] = v
			}
			nodes, err := i.handleRecordFeedback(callerContext(actOwner), call, 0)
			if err != nil {
				t.Fatalf("recordFeedback: %v", err)
			}
			data := rowMap(argsOf(t, eng, "createWorkObservation"), "data")
			if data["validatorDisagrees"] != tc.disagrees {
				t.Errorf("validatorDisagrees = %v, want %v", data["validatorDisagrees"], tc.disagrees)
			}
			if data["validatorObservationId"] != rowString(tc.decision, "id") {
				t.Errorf("validatorObservationId = %v, want the validator's decision", data["validatorObservationId"])
			}
			if decodeReply(t, nodes)["validatorDisagrees"] != tc.disagrees {
				t.Errorf("the reply does not carry the disagreement")
			}
		})
	}

	t.Run("a version the validator never judged records nothing about it", func(t *testing.T) {
		i, eng := feedbackFixture(t)
		eng.reply("workObservationsForOwnerRun", flagged)
		if _, err := i.handleRecordFeedback(callerContext(actOwner), map[string]any{
			"runId": actRunId, "stepKey": "draft", "version": float64(1), "verdict": "like",
		}, 0); err != nil {
			t.Fatalf("recordFeedback: %v", err)
		}
		data := rowMap(argsOf(t, eng, "createWorkObservation"), "data")
		if _, has := data["validatorDisagrees"]; has {
			t.Errorf("data = %v; the validator judged v2, not v1", data)
		}
	})
}

// validatorDecisionRow is the decision observation workValidateAnswer writes.
func validatorDecisionRow(id, stepKey string, version int, axes work.Axes, reason string) map[string]any {
	v := work.ValidatorVerdict{Axes: axes, Reason: reason}
	return map[string]any{
		"id": id, "runId": actRunId, "stepKey": stepKey, "kind": "decision",
		"createdAt": rfc(testNow),
		"data": map[string]any{
			"validator": map[string]any{"axes": axes.Object(), "reason": reason, "verdict": v.Word()},
			"target":    map[string]any{"stepKey": stepKey, "version": float64(version)},
			"level":     "strong",
			"model":     "chat54",
		},
	}
}

// A recording run's actions are steps with no step order, and a verdict on
// one is exactly what epic D's candidate gate reads -- so the act that judges
// a version does not ask for a place in the order, only for the version to be
// recorded.
func TestAVerdictOnARecordedActionIsRecorded(t *testing.T) {
	i, eng, store := newActsIntegration(t)
	addVersion(store, actRunId, "action-a1", 0, 1, "done", nil, nil)
	run := actRunRow(runStatusSucceeded)
	run["automationName"] = appSessionTemplate
	delete(run, "stepOrder")
	eng.reply("workRunForOwner", run)

	if _, err := i.handleRecordFeedback(callerContext(actOwner), map[string]any{
		"runId": actRunId, "stepKey": "action-a1", "verdict": "dislike", "product": true,
	}, 0); err != nil {
		t.Fatalf("recordFeedback on a recorded action: %v", err)
	}
	target := rowMap(rowMap(argsOf(t, eng, "createWorkObservation"), "data"), "target")
	if target["stepKey"] != "action-a1" || target["version"] != float64(1) {
		t.Errorf("target = %v", target)
	}
}
