package conformance

// conf_1727_test.go -- the run_automation event-binding dimension (#1727).
//
// #1706 made the triggering event a first-class, in-scope value the LogicRunner
// threads into every nested step's argument resolution -- but only on the LIVE
// execution path. The run_automation DRY-RUN path (the Gate-2 sandbox the MCP
// connector preview routes through) re-dispatched a forwarded `logic X { event:
// event }` step through a DIFFERENT, mutation-oriented arg resolver that (a)
// treated the bare `event` runtime-reference token as a literal string and (b)
// never unwrapped the single positional object arg. So the logic ran with
// args["event"] = "event" (a string) -- every nested args.event.payload.<field>
// navigated into a string and resolved to null, and the first step feeding such
// a reference to a @required argument failed with "required argument <x> is
// missing" (the verbatim Run-5 failure shape across all five event-trigger
// logics).
//
// This dimension drives EACH of the five logics through the genuine
// run_automation paths -- the LIVE path (Loader.LoadByName + Executor
// .ExecuteWithEvent, byte-for-byte what the MCP runner does) AND the DRY-RUN
// path (DSLConstructSource + RunBundleDryRun, the sandbox preview) -- with a
// representative event, and asserts the binding resolves: no step fails because
// an event.* reference came back null. It FAILS against the pre-#1727 sandbox
// and PASSES with the converged arg resolver.

import (
	"strings"
	"testing"
	"time"

	// init() registers the Gate-2 dry-run sandbox hook (SetSandboxDryRunner)
	// so RunBundleDryRun has a runner in this binary.
	_ "github.com/znasllc-io/memql/component/automations/steps"

	"github.com/znasllc-io/memql/component/automations"
	automationSteps "github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

func eventDryRunBindingCheck() check {
	return check{
		Issue:   "#1727",
		Dim:     "run-automation-event-binding",
		NeedsDB: true,
		Run:     runEventDryRunBinding,
	}
}

// bindingErrorSignatures are the substrings that mark a FAILURE CAUSED BY THE
// EVENT NOT BINDING into nested step scope. With the event bound, every
// @required argument these five logics feed from event.* is present, so any of
// these signatures appearing is a #1727 regression. Environmental failures that
// are NOT binding failures (an AI provider / integration the lightweight
// conformance engine does not register, the pre-existing ai() positional-arg
// render quirk) are explicitly tolerated -- they occur identically on the live
// path and land PAST the event-binding point.
var bindingErrorSignatures = []string{
	"is missing",      // "required argument <x> is missing" -- the Run-5 shape
	"must be string",  // "ensureDailySpaceForUser() 'userId' field must be string"
	"args.event",      // an unresolved args.event.* reference leaked
	"unresolved",      // generic unresolved-reference surface
	"expected object", // "argument \"event\": expected object, got string"
}

// allowedNonBindingFailures are the environmental failure substrings tolerated
// for the two logics whose end-to-end completion needs a provider/integration
// the conformance engine intentionally does not wire (kept light). These land
// PAST event binding, so they prove the binding resolved.
var allowedNonBindingFailures = []string{
	"unknown builtin executor", // integration.dailyspace.ensureForUser not registered here
	`function "ai"`,            // ai() positional-arg render quirk (pre-existing, live-path-identical)
	// generateResponse validates its (correctly-bound) siParticipantId
	// against a cognition participant row + actor write-authz the lightweight
	// conformance engine does not seed. Whether part-1727 happens to exist in
	// the shared conformance DB depends on test ordering, so this validation
	// failure surfaced intermittently and tripped the "unexpected error" arm
	// (#1759). It lands PAST event binding -- and both signatures are keyed on
	// the RESOLVED id "part-1727", so a genuine binding regression (a null-bound
	// id would yield `participant "" not found`) still fails rather than being
	// masked here.
	// The id renders either bare ("part-1727") or fully-qualified
	// ("v1:cognition:participant:part-1727") depending on which step surfaces the
	// failure (sendTextUtterance uses the canonical id) and on shared-DB
	// test ordering -- hence both forms are tolerated. Both still anchor on the
	// RESOLVED id, so a genuine null-bind regression (participant "" not found)
	// is not masked. Without the fully-qualified variants this dimension flaked
	// intermittently in the merge queue (memql#1821).
	`participant "part-1727" not found`,                          // getLatestParticipantPayload: no seeded participant row (bare id)
	`participant "v1:cognition:participant:part-1727" not found`, // sendTextUtterance: no seeded row (fully-qualified id)
	`as participant "part-1727"`,                                 // auth validation: no actor write-authz in conformance (bare id)
	`as participant "v1:cognition:participant:part-1727"`,        // auth validation (fully-qualified id)
}

func runEventDryRunBinding(t *testing.T, e *Env) {
	loader := automations.NewLoader(automations.LoaderOptions{Logger: e.Eng.Logger})
	exec := automations.NewExecutor(automations.ExecutorOptions{
		Logger:       e.Eng.Logger,
		Engine:       e.Eng,
		EventBus:     e.Eng.EventBus(),
		StepRegistry: automationSteps.NewRegistry(),
	})

	cases := []struct {
		logic string
		event map[string]any
	}{
		// (logicAutoJoinAI moved to the product pack with the space concept
		// in #2038/B2; the remaining cognition/data logics keep this coverage.)
		{"voiceMigrationOnSecondHuman", map[string]any{
			"id": "user-1727", "activePartitionId": "space-1727",
		}},
		{"generateResponse", map[string]any{
			"partitionId": "space-1727", "utteranceId": "utt-1727", "siParticipantId": "part-1727",
			"agentId": "agent-1727", "promptTemplateId": "cognitionReply", "promptData": map[string]any{},
		}},
		// (logicEnsureDailySpaceOnAuthSession moved to the product pack in
		// #1976; the remaining cognition/data logics keep this coverage.)
		{"conflictDetection", map[string]any{
			"id": "rec-1727", "partitionId": "space-1727", "recordType": "contact",
			"naturalKeyField": "email", "naturalKeyValue": "a@example.com",
		}},
	}

	for _, c := range cases {
		auto, err := loader.LoadByName(c.logic)
		if err != nil {
			t.Fatalf("#1727: LoadByName(%q) -- the logic must resolve to a runnable entry point: %v", c.logic, err)
		}

		// LIVE path -- exactly what the MCP run_automation runner does (#1706 fixed
		// this; lock it from the run_automation surface).
		ev := &events.Event{Topic: "mcp.run." + auto.Name, Kind: events.KindNodeCreated, Payload: c.event, Timestamp: time.Now().UTC()}
		execn, liveErr := exec.ExecuteWithEvent(e.Ctx, auto, "mcp", ev)
		liveMsg := ""
		if liveErr != nil {
			liveMsg = liveErr.Error()
		} else if execn != nil {
			liveMsg = execn.Error
		}
		assertNoBindingFailure(t, c.logic, "LIVE", liveMsg)

		// DRY-RUN path -- the #1727-broken sandbox preview. Source resolved by the
		// SAME canonical name the runner uses.
		src, ok := memql.DSLConstructSource(e.Eng.Logger, "automation", auto.Name)
		if !ok {
			t.Fatalf("#1727: DSLConstructSource(automation, %q) not found -- dry-run needs the source", auto.Name)
		}
		report, drErr := memql.RunBundleDryRun(e.Ctx, e.Eng, memql.DryRunRequest{
			AutomationName:   auto.Name,
			AutomationSource: src,
			TriggerEvent:     &memql.DryRunTriggerEvent{Topic: "mcp.run." + auto.Name, Kind: "manual", Payload: c.event},
			Mode:             memql.DryRunModeIsolated,
		})
		if drErr != nil {
			t.Fatalf("#1727: RunBundleDryRun(%q) returned a hard error: %v", c.logic, drErr)
		}
		assertNoBindingFailure(t, c.logic, "DRY-RUN", report.FailureReason)

		t.Logf("#1727: %s bound the triggering event into nested step scope on both run_automation paths", c.logic)
	}
}

// assertNoBindingFailure fails the test if msg carries an event-binding-failure
// signature. An empty msg (clean run) trivially passes. A non-empty msg passes
// only when it is one of the tolerated environmental (non-binding) failures.
func assertNoBindingFailure(t *testing.T, logic, path, msg string) {
	t.Helper()
	if strings.TrimSpace(msg) == "" {
		return
	}
	for _, sig := range bindingErrorSignatures {
		if strings.Contains(msg, sig) {
			t.Fatalf("#1727 regression: %s (%s) failed because an event.* reference did not bind into nested step scope: %s", logic, path, msg)
		}
	}
	for _, allowed := range allowedNonBindingFailures {
		if strings.Contains(msg, allowed) {
			return // environmental, past the binding point
		}
	}
	t.Fatalf("#1727: %s (%s) failed with an unexpected error (not a tolerated environmental failure): %s", logic, path, msg)
}
