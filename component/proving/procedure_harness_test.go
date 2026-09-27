package proving

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/work"
	procedure "github.com/znasllc-io/memql/integrations/procedure"
)

// The engine harness's pieces that need no database. The harness as a whole
// runs in the proving lane against a real Postgres; these pin the translations
// it makes on the way, where a wrong field would read as the platform's fault.

func TestTheOverlayKeepsWhatTheScenarioDidNotWrite(t *testing.T) {
	deployment := work.LadderPolicy{ShadowMatches: 7, DistinctBindings: 3, CanaryMatches: 4, FailuresToDemote: 3, InsufficientToDemote: 2, RetireAfterDays: 60}
	got := overlayPolicy(deployment, work.LadderPolicy{ShadowMatches: 2, CanaryMatches: 1})
	want := work.LadderPolicy{ShadowMatches: 2, DistinctBindings: 3, CanaryMatches: 1, FailuresToDemote: 3, InsufficientToDemote: 2, RetireAfterDays: 60}
	if got != want {
		t.Fatalf("overlay = %+v, want %+v: a value the scenario left out is the deployment's", got, want)
	}
	// No row at all: the defaults the seed carries, under the overlay.
	if got := overlayPolicy(work.LadderPolicy{}, work.LadderPolicy{ShadowMatches: 2}); got.ShadowMatches != 2 || got.CanaryMatches != work.DefaultLadderPolicy().CanaryMatches {
		t.Fatalf("overlay over no row = %+v", got)
	}
}

func TestALadderValueThatIsNotAWholeNumberReadsAsZero(t *testing.T) {
	// Zero is never a stored value (the schema's floor is one), so the
	// read-back comparison refuses it rather than taking it for a number
	// anybody wrote.
	for in, want := range map[any]int{2.0: 2, 2.5: 0, int64(3): 3, 4: 4, "5": 0, nil: 0} {
		if got := intField(map[string]any{"v": in}, "v"); got != want {
			t.Errorf("intField(%v) = %d, want %d", in, got, want)
		}
	}
}

func TestTheDispatcherReportsWhatTheProvingMachineDid(t *testing.T) {
	s := lifecycles(t)[scnDivergence]
	w := newWorld(s)
	pw := newProcedureWorld(s, w)
	d := worldDispatcher{w: pw}

	res, err := d.Dispatch(context.Background(), procedure.DispatchRequest{
		Tool: "exec", IdempotencyKey: "r:step0:1", Args: map[string]any{"command": "notify.sh --to ops --account acme"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	o := res.Observation
	if o.IsError == nil || *o.IsError || o.ExitCode == nil || *o.ExitCode != 0 || o.ResultType != "string" || !res.Delivered {
		t.Fatalf("a clean command reported %+v delivered=%v; every dispatcher reports the error flag, the exit code and the type", o, res.Delivered)
	}
	if w.Count("machine") != 1 {
		t.Fatal("a dispatched command did not reach the proving machine")
	}

	// A shadow's step runs in the sandbox: answered, never delivered.
	res, err = d.Dispatch(context.Background(), procedure.DispatchRequest{
		Tool: "exec", Sandbox: true, IdempotencyKey: "s:step0:1", Args: map[string]any{"command": []any{"notify.sh", "--to", "ops", "--account", "acme"}},
	})
	if err != nil || res.Delivered || w.Count("machine") != 1 {
		t.Fatalf("a sandboxed argv command: err %v delivered %v, machine count %d", err, res.Delivered, w.Count("machine"))
	}

	// A tool the proving world has no machine for did NOT run: a Go error.
	if _, err := d.Dispatch(context.Background(), procedure.DispatchRequest{Tool: "fs_write", Args: map[string]any{"file_path": "x"}}); err == nil {
		t.Fatal("the dispatcher claimed to run a file write the proving world cannot perform")
	}
}

func TestTheProberAnswersOnlyWhatTheMachineHas(t *testing.T) {
	s := lifecycles(t)[scnDivergence]
	p := worldProber{w: newProcedureWorld(s, newWorld(s))}
	got, err := p.Probe(context.Background(), work.TargetWorkbench, "owner", "run",
		procedure.Preconditions{Tools: map[string]string{"notify.sh": "1.0.0", "missing.sh": "1.0.0"}})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got.Tools["notify.sh"] != provingToolVersion {
		t.Errorf("a tool the machine has reads %q", got.Tools["notify.sh"])
	}
	if _, measured := got.Tools["missing.sh"]; measured {
		t.Error("the prober answered for a tool the machine does not have; an unmeasured precondition must stay absent")
	}
	if got.EmptyWorkspace == nil || !*got.EmptyWorkspace {
		t.Error("a replay's fresh workspace was not reported empty")
	}
}

func TestTheFallbackHandsTheAppTheGuidanceItWasGiven(t *testing.T) {
	s := lifecycles(t)[scnDivergence]
	pw := newProcedureWorld(s, newWorld(s))
	app := newFixtureApp(pw, s.Steps, nil, "owner")
	app.expect(appGoal{OwnerUserId: "owner", GoalRunId: "goal-run", Statement: s.Goal, Variables: map[string]string{"account": "acme"}})
	f := appFallback{app: app}
	if _, err := f.Handover(context.Background(), procedure.FallbackRequest{
		OwnerUserId: "owner", RunId: "goal-run",
		Guidance: procedure.Guidance{Completed: []procedure.CompletedStep{{Index: 0, Tool: "exec", IdempotencyKey: "k0", SideEffect: true}}, Prompt: "guidance"},
	}); err != nil {
		t.Fatalf("Handover: %v", err)
	}
	h := app.handoversSince(0)
	if len(h) != 1 || len(h[0].Order.Completed) != 1 || h[0].Order.Completed[0].IdempotencyKey != "k0" || h[0].Order.Prompt != "guidance" {
		t.Fatalf("the app was handed %+v", h)
	}
	if got := h[0].Session.Performed; len(got) != len(s.Steps)-1 {
		t.Fatalf("the app performed %v; the completed step was not honoured", got)
	}
}

func TestAReplayOutcomeKeepsEveryFieldTheDriverReads(t *testing.T) {
	rep := replayReportOf("c1", "trusted", procedure.ReplayOutcome{
		Served: true, Diverged: true, DivergedStep: 1, Diagnosis: "why", StartRefused: true, FellBack: true,
		ModelCalls: 2, ApprovalId: "a1", ReplayRunId: "r1", Code: "procedure_diverged", NotCompared: true,
		Rung:       work.RungTrusted,
		Transition: work.Transition{From: work.RungTrusted, To: work.RungShadow},
		Completed:  []procedure.CompletedStep{{Index: 0, Tool: "exec", IdempotencyKey: "k", SideEffect: true}},
	})
	want := ReplayReport{
		ConstructId: "c1", Mode: "trusted", Served: true, Diverged: true, DivergedStep: 1, Diagnosis: "why",
		StartRefused: true, FellBack: true, ModelCalls: 2, Proposed: true, PromotionApprovalId: "a1",
		From: "trusted", To: "shadow", Rung: "trusted", ReplayRunId: "r1", Code: "procedure_diverged", NotCompared: true,
		Completed: []CompletedReport{{Index: 0, Tool: "exec", IdempotencyKey: "k", SideEffect: true}},
	}
	if rep.ConstructId != want.ConstructId || rep.Served != want.Served || rep.Diverged != want.Diverged ||
		rep.DivergedStep != want.DivergedStep || rep.Diagnosis != want.Diagnosis || rep.StartRefused != want.StartRefused ||
		rep.FellBack != want.FellBack || rep.ModelCalls != want.ModelCalls || rep.Proposed != want.Proposed ||
		rep.PromotionApprovalId != want.PromotionApprovalId || rep.From != want.From || rep.To != want.To ||
		rep.Rung != want.Rung || rep.ReplayRunId != want.ReplayRunId || rep.Code != want.Code ||
		rep.NotCompared != want.NotCompared || len(rep.Completed) != 1 || rep.Completed[0] != want.Completed[0] {
		t.Fatalf("mapped %+v\nwant   %+v", rep, want)
	}
}

func TestAPromotionIsReadOffItsApprovalRowAndNothingElseIs(t *testing.T) {
	a, ok := promotionState(map[string]any{"id": "v1:work:approval:1", "kind": "procedurePromotion", "decision": "approved", "subject": map[string]any{"constructId": "c1"}})
	if !ok || a.ApprovalId != "v1:work:approval:1" || a.ConstructId != "c1" || a.Decision != "approved" {
		t.Fatalf("promotionState = %+v, %v", a, ok)
	}
	if _, ok := promotionState(map[string]any{"id": "x", "kind": "feedback"}); ok {
		t.Fatal("an approval of another kind was read as a promotion")
	}
}

func TestTheWarningLogKeepsEveryWarningAndNothingQuieter(t *testing.T) {
	log := newWarningLog(nil)
	l := log.logger().With("construct", "c1")
	l.Info("routine")
	l.Warn("could not write", "error", "refused")
	l.Error("worse")
	got := log.warnings()
	if len(got) != 2 || !strings.Contains(got[0], "could not write") || !strings.Contains(got[0], "construct=c1") || !strings.Contains(got[0], "error=refused") {
		t.Fatalf("warnings = %q", got)
	}
	// And it still passes records on to the bench's own logger.
	var seen []string
	next := slog.New(recordingHandler{seen: &seen})
	fwd := newWarningLog(next).logger()
	fwd.Info("passed on")
	if len(seen) != 1 {
		t.Fatalf("the bench's own logger saw %d records, want 1", len(seen))
	}
}

type recordingHandler struct{ seen *[]string }

func (h recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h recordingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.seen = append(*h.seen, r.Message)
	return nil
}
func (h recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordingHandler) WithGroup(string) slog.Handler      { return h }
