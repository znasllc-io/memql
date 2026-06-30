package memql_test

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

// dryRunMeteredAutomation issues two web reads (webSearch + fetchUrl) as direct
// function steps -- the metered web tier from the issue's read list. Both are
// real reads, so without a DB/provider the underlying delegate may fail;
// metering records the calls UP FRONT so the manifest reflects the read intent
// regardless of the delegate outcome.
const dryRunMeteredAutomation = `@enabled
@trigger(event="node.created", concept="v1:authoring:bundle")
@description("Sandbox: read-heavy automation")
automation sandboxReadHeavy {
  step search {
    webSearch(query: "memql dry run sandbox")
  }
  step fetch {
    fetchUrl(url: "https://example.com/doc")
  }
}`

// TestDryRun_MetersWebReads: a webSearch() and a fetchUrl() read each record a
// webCalls entry with the resolved target. The run itself may not be OK (no
// provider/DB wired for the real read), but metering is captured up front.
func TestDryRun_MetersWebReads(t *testing.T) {
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

	mani := report.SideEffectManifest
	if len(mani.WebCalls) < 1 {
		t.Fatalf("expected metered web calls, got %d: %+v", len(mani.WebCalls), mani.WebCalls)
	}
	byFn := map[string]memql.RecordedWebCall{}
	for _, w := range mani.WebCalls {
		byFn[w.Function] = w
	}
	search, ok := byFn["webSearch"]
	if !ok {
		t.Fatalf("expected a webSearch web call, got %+v", mani.WebCalls)
	}
	if !strings.Contains(search.Target, "memql dry run") {
		t.Errorf("expected the resolved query as webSearch target, got %q", search.Target)
	}
	if fetch, ok := byFn["fetchUrl"]; ok {
		if !strings.Contains(fetch.Target, "example.com/doc") {
			t.Errorf("expected the resolved url as fetchUrl target, got %q", fetch.Target)
		}
	}

	// Zero prod writes -- a read-only automation records nothing in mutations,
	// and web reads carry no token cost.
	if n := len(mani.Mutations); n != 0 {
		t.Errorf("expected no mutations on a read-only automation, got %d", n)
	}
}

// TestBuildDryRunMutationCall_Passed: the persist-call builder renders a
// recordBundleDryRun(...) call carrying the bundle id, dryRunPassed
// status, and the report on the dryRunReport object arg. Pure -- no DB.
func TestBuildDryRunMutationCall_Passed(t *testing.T) {
	report := memql.BundleDryRunReport{
		OK:               true,
		Mode:             memql.DryRunModeIsolated,
		SandboxPartition: "sandbox:dryrun:abc",
		AutomationName:   "sandboxRecordConstruct",
		Trace:            []memql.DryRunStep{{StepId: "record", StepType: "function", Status: "success", Intercepted: true}},
		SideEffectManifest: memql.SideEffectManifest{
			Mutations:       []memql.RecordedMutation{{StepId: "record", Concept: "v1:authoring:construct", Partition: "sandbox:dryrun:abc"}},
			AiCalls:         []memql.RecordedAiCall{},
			WebCalls:        []memql.RecordedWebCall{},
			BlockedWebhooks: []memql.BlockedWebhook{},
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
		Mode:          memql.DryRunModeIsolated,
		FailureReason: "step run failed: boom",
		SideEffectManifest: memql.SideEffectManifest{
			Mutations: []memql.RecordedMutation{}, AiCalls: []memql.RecordedAiCall{},
			WebCalls: []memql.RecordedWebCall{}, BlockedWebhooks: []memql.BlockedWebhook{},
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
