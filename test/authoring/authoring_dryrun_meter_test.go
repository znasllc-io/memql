package authoring

// authoring_dryrun_meter_test.go -- Gate-2 increment 2 coverage: read metering
// (si / web reads) + cost estimate + the recordBundleDryRun persist
// path.
//
// External test package; links component/automations/steps (the dry-run bridge)
// alongside component/memql, and reuses newDryRunEngine from
// authoring_dryrun_test.go (full engine Init, no DB).

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"

	_ "github.com/znasllc-io/memql/component/automations/steps"
)

// dryRunMeteredAutomation requests integration web reads. These executors have
// no preview classification and must be refused before execution or metering.
const dryRunMeteredAutomation = `@enabled
@trigger(event="node.created", concept="v1:authoring:bundle")
@description("Sandbox: read-heavy automation")
automation sandboxReadHeavy {
  search := builtin webSearch(query: "memql dry run sandbox")
  fetch := builtin fetchUrl(url: "https://example.com/doc")
}`

// TestDryRun_RefusesUnclassifiedWebReads drives the full automation executor
// so a refused builtin must produce a failed report and trace without a panic.
func TestDryRun_RefusesUnclassifiedWebReads(t *testing.T) {
	eng := newDryRunEngine(t)

	report, err := memql.RunBundleDryRun(t.Context(), eng, memql.DryRunRequest{
		BundleId:         "b-meter-1",
		AutomationName:   "sandboxReadHeavy",
		AutomationSource: dryRunMeteredAutomation,
		TriggerEvent:     &memql.DryRunTriggerEvent{Topic: "graph.node.created.default.v1:authoring:bundle"},
	})
	if err != nil {
		t.Fatalf("RunBundleDryRun returned error: %v", err)
	}

	if report.OK || !strings.Contains(report.FailureReason, "not classified as side-effect free") {
		t.Fatalf("expected builtin refusal, got %+v", report)
	}
	if len(report.Trace) != 2 || report.Trace[0].Status != "failed" || !report.Trace[0].Intercepted || report.Trace[1].Status != "notRun" {
		t.Fatalf("expected a refused first step and an unrun second step, got %+v", report.Trace)
	}
	mani := report.SideEffectManifest
	if len(mani.WebCalls) != 0 || len(mani.Mutations) != 0 {
		t.Fatalf("refused builtin must not record performed reads or writes: %+v", mani)
	}
}

// TestBuildDryRunMutationCall_Passed: the persist-call builder renders a
// recordBundleDryRun(...) call carrying the bundle id, dryRunPassed
// status, and the report on the dryRunReport object arg. Pure -- no DB.
func TestBuildDryRunMutationCall_Passed(t *testing.T) {
	report := memql.BundleDryRunReport{
		OK:               true,
		SandboxPartition: "sandbox:dryrun:abc",
		AutomationName:   "sandboxRecordConstruct",
		Trace:            []memql.DryRunStep{{StepId: "record", StepType: "function", Status: "success", Intercepted: true}},
		SideEffectManifest: memql.SideEffectManifest{
			Mutations: []memql.RecordedMutation{{StepId: "record", Concept: "v1:authoring:construct", Partition: "sandbox:dryrun:abc"}},
			AiCalls:   []memql.RecordedAiCall{},
			WebCalls:  []memql.RecordedWebCall{},
		},
		CostEstimate: memql.CostEstimate{Tokens: 0, Usd: 0},
	}
	call, err := memql.BuildDryRunMutationCall("b-1", report)
	if err != nil {
		t.Fatalf("BuildDryRunMutationCall: %v", err)
	}
	for _, want := range []string{
		"recordBundleDryRun(",
		`"bundleId": "b-1"`,
		`"status": "dryRunPassed"`,
		`"dryRunReport":`,
		`"sandboxPartition": "sandbox:dryrun:abc"`,
		`"v1:authoring:construct"`,
	} {
		if !strings.Contains(call, want) {
			t.Errorf("rendered call missing %q\ngot: %s", want, call)
		}
	}
	// A passed run carries no failureReason arg.
	if strings.Contains(call, "failureReason") {
		t.Errorf("passed run should not carry failureReason, got: %s", call)
	}
}

// TestBuildDryRunMutationCall_Failed: a failed run renders status=failed and
// carries the failureReason.
func TestBuildDryRunMutationCall_Failed(t *testing.T) {
	report := memql.BundleDryRunReport{
		OK:            false,
		FailureReason: "step run failed: boom",
		SideEffectManifest: memql.SideEffectManifest{
			Mutations: []memql.RecordedMutation{}, AiCalls: []memql.RecordedAiCall{},
			WebCalls: []memql.RecordedWebCall{},
		},
	}
	call, err := memql.BuildDryRunMutationCall("b-2", report)
	if err != nil {
		t.Fatalf("BuildDryRunMutationCall: %v", err)
	}
	if !strings.Contains(call, `"status": "failed"`) {
		t.Errorf("expected status=failed, got: %s", call)
	}
	if !strings.Contains(call, `"failureReason": "step run failed: boom"`) {
		t.Errorf("expected failureReason carried, got: %s", call)
	}
}

// TestBuildDryRunMutationCall_RequiresBundleId guards the bundle-id precondition.
func TestBuildDryRunMutationCall_RequiresBundleId(t *testing.T) {
	if _, err := memql.BuildDryRunMutationCall("", memql.BundleDryRunReport{}); err == nil {
		t.Fatal("expected an error for an empty bundleId")
	}
}
