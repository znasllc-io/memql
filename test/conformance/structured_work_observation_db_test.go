package conformance

// structured_work_observation_db_test.go -- a STRUCTURED call made inside a
// work step now takes part in the step's progress journal and its Stop.
//
// Before the structured surface walked the route (component/router
// fallback_structured.go), a structured call emitted no call observation, so
// inside ObserveWorkCalls -- compile on the planner (goal triage among it),
// an agent turn, an owned goal step -- it wrote no progress snapshot and a
// person's Stop could not reach it: the step ran to its end. It now behaves
// as every chat call there already did, which costs each structured call a
// synchronous workRunForOwner read and a progress write before it starts, a
// second progress write when it ends, and a 2-second cancellation poll while
// it runs. These pin that behaviour for real, against the database the reads
// and writes go to:
//   - a finished call leaves one progress snapshot on its run and step;
//   - a Stop requested while the call runs stops it, with the person's
//     cancellation as the cause;
//   - a call whose actor is not the run's owner is stopped before the source
//     answers: the cancellation read, made as that actor, cannot see the run,
//     and the progress write refuses an actor that is not the owner -- the
//     journal's rule, which structured calls now share.
//
// Green-by-skip warning: without a database this skips. To verify for real:
//
//	MEMQL_DATABASE_DSN=... MEMQL_REQUIRE_DB=1 go test -count=1 \
//	  -run 'TestAStructuredCallInsideAWorkStep' ./test/conformance/

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// blockingVendor answers a structured call at once, or -- when hold is set --
// only when the call's context ends, which is how a model call that a person
// stops looks from the provider's side. Like every real provider it refuses a
// call whose context has already ended, and counts the calls it answered.
type blockingVendor struct {
	hold     bool
	started  chan struct{}
	answered int
}

func (v *blockingVendor) Call(context.Context, string) (any, error) { return "", nil }
func (v *blockingVendor) CallChatStructured(ctx context.Context, _ []common.ChatMessage, _ common.StructuredSchema) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if v.started != nil {
		close(v.started)
	}
	if !v.hold {
		v.answered++
		return `{"complexity":"trivial"}`, nil
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(30 * time.Second):
		return "", errors.New("the held call was never stopped")
	}
}

// workStepRig is one owned run, a router whose only source is the vendor, and
// the ObserveWorkCalls context a work step makes its calls under.
type workStepRig struct {
	env    *Env
	owner  string
	runId  string
	router *router.Router
}

func newWorkStepRig(t *testing.T, vendor *blockingVendor) *workStepRig {
	t.Helper()
	env := newEnv(t)
	if !env.HasDB {
		if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
			t.Fatal("the work journal needs Postgres, and MEMQL_REQUIRE_DB=1 makes its absence a failure")
		}
		t.Skip("the work journal needs Postgres (MEMQL_DATABASE_DSN)")
	}
	stamp := time.Now().UnixNano()
	rig := &workStepRig{env: env, owner: fmt.Sprintf("user-structobs-%d", stamp), runId: fmt.Sprintf("structobs-%d", stamp)}
	ownerCtx := auth.ContextWithUserActor(context.Background(), rig.owner)
	if _, err := env.Eng.Execute(auth.ContextWithInternalOrigin(ownerCtx), "mutation "+mustRender(t, "createWorkRun", map[string]any{
		"runId": rig.runId, "automationName": "structuredObservationProbe", "templateFingerprint": "structobs",
		"status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano),
	})); err != nil {
		t.Fatalf("open the run: %v", err)
	}

	providers := memql.NewProviderRegistryForTest()
	providers.RegisterWithParamsForTest("streamClaudeSonnet", "AnthropicStream", "claude-sonnet",
		map[string]any{"contextWindow": 200000}, vendor)
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"vendor": {"streamClaudeSonnet"}})
	rules := memql.NewRuleRegistry()
	if err := rules.Register(&memql.RuleConfig{
		Name: memql.DefaultRuleName, When: memql.RuleWhen{Present: map[string]bool{}}, Policy: "vendor",
		OnUnavailable: memql.OnUnavailableDegrade, Locked: true, SourceFile: "dsl/rules/rules.memql",
	}); err != nil {
		t.Fatal(err)
	}
	if err := rules.Finalize(); err != nil {
		t.Fatal(err)
	}
	rig.router = router.New(providers, policies, rules, nil, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	return rig
}

func mustRender(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	call, err := langparser.RenderCall(name, args)
	if err != nil {
		t.Fatal(err)
	}
	return call
}

// stepContext is the context a work step makes its calls under, as the
// planner's compile and the executor's owned step build it: the run, the
// actor, and ObserveWorkCalls over both.
func (rig *workStepRig) stepContext(actor string) (context.Context, context.CancelCauseFunc) {
	ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), actor), common.RunContext{
		RunId: rig.runId, StepKey: "triage", OwnerUserId: rig.owner, Mode: common.RunModeLive,
	})
	ctx, cancel := context.WithCancelCause(ctx)
	return rig.env.Eng.ObserveWorkCalls(ctx, cancel), cancel
}

func (rig *workStepRig) call(t *testing.T, ctx context.Context) error {
	t.Helper()
	resolved, err := rig.router.ResolveFor(ctx, router.ResolveRequest{
		Level: airoute.LevelFast, Modality: airoute.ModalityStructured, PromptName: "goalComplexityTriage",
		Needs: airoute.Needs{MinContextTokens: 8000, Structured: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolved.Client.(common.ChatStructuredProvider).CallChatStructured(ctx,
		[]common.ChatMessage{{Role: "user", Content: "classify"}}, common.StructuredSchema{Name: "triage"})
	return err
}

func (rig *workStepRig) progress(t *testing.T) []map[string]any {
	t.Helper()
	res, err := rig.env.Eng.Execute(auth.ContextWithUserActor(context.Background(), rig.owner),
		"query "+mustRender(t, "workObservationsForOwnerRun", map[string]any{"runId": rig.runId}))
	if err != nil {
		t.Fatalf("read the run's observations: %v", err)
	}
	var out []map[string]any
	for _, row := range memql.MaterializeRows(res) {
		if strings.HasPrefix(fmt.Sprint(row["content"]), "Execution progress: model") {
			out = append(out, row)
		}
	}
	return out
}

func TestAStructuredCallInsideAWorkStepLeavesAProgressSnapshot(t *testing.T) {
	rig := newWorkStepRig(t, &blockingVendor{})
	ctx, cancel := rig.stepContext(rig.owner)
	defer cancel(nil)
	if err := rig.call(t, ctx); err != nil {
		t.Fatalf("the structured call failed inside the step: %v", err)
	}
	snapshots := rig.progress(t)
	if len(snapshots) != 1 {
		t.Fatalf("the run has %d model progress snapshot(s), want one updated in place: %v", len(snapshots), snapshots)
	}
	data, _ := snapshots[0]["data"].(map[string]any)
	execution, _ := data["execution"].(map[string]any)
	if execution["phase"] != "completed" || execution["provider"] != "streamClaudeSonnet" {
		t.Errorf("the snapshot's execution = %v, want the completed call on the source that served it", execution)
	}
	if snapshots[0]["stepKey"] != "triage" {
		t.Errorf("the snapshot is on step %v, want the step the call was made in", snapshots[0]["stepKey"])
	}
}

func TestAStructuredCallInsideAWorkStepStopsWhenThePersonStopsTheRun(t *testing.T) {
	vendor := &blockingVendor{hold: true, started: make(chan struct{})}
	rig := newWorkStepRig(t, vendor)
	ctx, cancel := rig.stepContext(rig.owner)
	defer cancel(nil)

	done := make(chan error, 1)
	go func() { done <- rig.call(t, ctx) }()
	select {
	case <-vendor.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the held call never reached the vendor")
	}
	ownerCtx := auth.ContextWithUserActor(context.Background(), rig.owner)
	if _, err := rig.env.Eng.Execute(auth.ContextWithInternalOrigin(ownerCtx), "mutation "+mustRender(t, "updateWorkRun", map[string]any{
		"runId": rig.runId, "cancelRequested": true, "cancelledBy": rig.owner,
	})); err != nil {
		t.Fatalf("request the stop: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the stopped call returned an answer")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a person's Stop did not reach the structured call within its poll")
	}
	var stopped *memql.WorkCancelledError
	if cause := context.Cause(ctx); !errors.As(cause, &stopped) || stopped.By != rig.owner {
		t.Errorf("the call's cause = %v, want the person's cancellation", cause)
	}
}

func TestAStructuredCallInsideAWorkStepNeedsTheRunOwnersAuthority(t *testing.T) {
	vendor := &blockingVendor{}
	rig := newWorkStepRig(t, vendor)
	ctx, cancel := rig.stepContext("user-not-the-owner")
	defer cancel(nil)
	if err := rig.call(t, ctx); err == nil {
		t.Fatal("a structured call made in a step without its run owner's authority was answered")
	}
	cause := context.Cause(ctx)
	if cause == nil || !(strings.Contains(cause.Error(), "no longer available") || strings.Contains(cause.Error(), "run owner's authority")) {
		t.Errorf("cause = %v, want the journal's refusal of an actor that is not the run's owner", cause)
	}
	if vendor.answered != 0 {
		t.Errorf("the source answered %d call(s) for an actor that is not the run's owner", vendor.answered)
	}
}
