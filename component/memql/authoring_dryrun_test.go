package memql_test

// authoring_dryrun_test.go -- engine-backed integration coverage for the Gate-2
// behavioral dry-run sandbox (issue #958), increment 1: ephemeral sandbox
// partition + mutation isolation + webhook record-and-block + behavioral trace.
//
// External test package so it can link component/automations/steps (whose
// init() registers the dry-run hook) alongside component/memql -- mirroring how
// authoring_sandbox_automation_test.go wires the Gate-1 hook. The full engine is
// Init'd over the embedded DSL tree (no Postgres) so the function registry the
// interception layer classifies mutations against is populated for real.

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"

	// Side-effect import: registers the behavioral dry-run runner the Gate-2
	// entry point dispatches to, and (transitively) the Gate-1 compile hook.
	_ "github.com/znasllc-io/memql/component/automations/steps"
)

// newDryRunEngine boots a full engine over the embedded DSL tree, exactly like
// the engine-init smoke test. No DB is wired -- which is load-bearing for the
// isolation proof: a mutation that ESCAPED interception would hit engine.Execute
// and fail on the missing DB, so a clean run proves the write was isolated.
func newDryRunEngine(t *testing.T) *memql.MemQLEngine {
	t.Helper()
	if err := concept.LoadConcepts(nil); err != nil {
		t.Fatalf("LoadConcepts (legacy embedded tree): %v", err)
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts (dsl/ domain-first tree): %v", err)
	}
	registry := concept.DefaultRegistry()
	if registry == nil || len(registry.List()) == 0 {
		t.Fatal("concept registry empty after load")
	}
	eng, err := memql.New(nil)
	if err != nil {
		t.Fatalf("memql.New: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine.Init: %v", err)
	}
	return eng
}

// dryRunMutationAutomation is a candidate automation that calls a REAL mutation
// function (mutationCreateAuthoringConstruct) -- exactly the authored-automation
// write shape Gate 2 must isolate. Driven by a synthetic trigger event.
const dryRunMutationAutomation = `@enabled
@trigger(event="node.created", concept="v1:authoring:bundle")
@description("Sandbox: record a construct when a bundle is created")
automation sandboxRecordConstruct {
  step record {
    mutationCreateAuthoringConstruct({
      constructId: "c-dryrun-1",
      bundleId: "b-dryrun-1",
      kind: "automation",
      name: "someAutomation",
      targetNamespace: "cognition",
      source: "automation someAutomation { }"
    })
  }
}`

// TestDryRun_MutationIsolatedToSandboxPartition: a dry-run of an automation that
// calls a real mutation function records the would-be write into the manifest
// under the ephemeral sandbox partition, returns OK, and produces ZERO prod
// side effects (proven by the no-DB engine: an un-isolated write would fail).
func TestDryRun_MutationIsolatedToSandboxPartition(t *testing.T) {
	eng := newDryRunEngine(t)

	report, err := memql.RunBundleDryRun(context.Background(), eng, memql.DryRunRequest{
		BundleId:         "b-dryrun-1",
		AutomationName:   "sandboxRecordConstruct",
		AutomationSource: dryRunMutationAutomation,
		TriggerEvent: &memql.DryRunTriggerEvent{
			Topic:   "graph.node.created.default.v1:authoring:bundle",
			Payload: map[string]any{"id": "b-dryrun-1"},
		},
	})
	if err != nil {
		t.Fatalf("RunBundleDryRun returned error: %v", err)
	}
	if !report.OK {
		t.Fatalf("expected dry-run OK, got failure: %s\ntrace=%+v", report.FailureReason, report.Trace)
	}
	if !strings.HasPrefix(report.SandboxPartition, "sandbox:dryrun:") {
		t.Errorf("expected an ephemeral sandbox partition, got %q", report.SandboxPartition)
	}
	if report.Mode != memql.DryRunModeIsolated {
		t.Errorf("expected isolated mode, got %q", report.Mode)
	}

	// The mutation must be recorded -- isolated, never persisted.
	muts := report.SideEffectManifest.Mutations
	if len(muts) != 1 {
		t.Fatalf("expected exactly 1 recorded mutation, got %d: %+v", len(muts), muts)
	}
	rec := muts[0]
	if rec.Partition != report.SandboxPartition {
		t.Errorf("recorded mutation partition %q != sandbox partition %q", rec.Partition, report.SandboxPartition)
	}
	if rec.Concept != "v1:authoring:construct" {
		t.Errorf("expected recorded concept v1:authoring:construct, got %q", rec.Concept)
	}
	if rec.Payload["name"] != "someAutomation" {
		t.Errorf("expected recorded payload name=someAutomation, got %+v", rec.Payload)
	}

	// The trace must mark the step as intercepted.
	if len(report.Trace) != 1 {
		t.Fatalf("expected 1 trace step, got %d: %+v", len(report.Trace), report.Trace)
	}
	step := report.Trace[0]
	if step.StepId != "record" || !step.Intercepted || step.Status != "success" {
		t.Errorf("expected intercepted success step 'record', got %+v", step)
	}
	if !strings.Contains(step.Note, "sandbox partition") {
		t.Errorf("expected isolation note on trace step, got %q", step.Note)
	}
}

// dryRunWebhookAutomation calls a webhook step -- an external POST the sandbox
// must record-and-block.
const dryRunWebhookAutomation = `@enabled
@trigger(event="node.created", concept="v1:authoring:bundle")
@description("Sandbox: POST to an external sink")
automation sandboxWebhook {
  step notify {
    webhook({
      url: "https://example.com/hook",
      method: "POST",
      body: { hello: "world" }
    })
  }
}`

// TestDryRun_WebhookRecordedAndBlocked: a dry-run of an automation with a
// webhook step records the request as a blocked external POST and dispatches
// nothing.
func TestDryRun_WebhookRecordedAndBlocked(t *testing.T) {
	eng := newDryRunEngine(t)

	report, err := memql.RunBundleDryRun(context.Background(), eng, memql.DryRunRequest{
		BundleId:         "b-dryrun-2",
		AutomationName:   "sandboxWebhook",
		AutomationSource: dryRunWebhookAutomation,
		TriggerEvent:     &memql.DryRunTriggerEvent{Topic: "graph.node.created.default.v1:authoring:bundle"},
	})
	if err != nil {
		t.Fatalf("RunBundleDryRun returned error: %v", err)
	}
	if !report.OK {
		t.Fatalf("expected dry-run OK, got failure: %s", report.FailureReason)
	}

	blocked := report.SideEffectManifest.BlockedWebhooks
	if len(blocked) != 1 {
		t.Fatalf("expected exactly 1 blocked webhook, got %d: %+v", len(blocked), blocked)
	}
	wh := blocked[0]
	if wh.Method != "POST" || !strings.Contains(wh.Url, "example.com/hook") {
		t.Errorf("unexpected blocked webhook: %+v", wh)
	}
	if wh.Body["hello"] != "world" {
		t.Errorf("expected captured webhook body hello=world, got %+v", wh.Body)
	}

	// No mutations on this run.
	if n := len(report.SideEffectManifest.Mutations); n != 0 {
		t.Errorf("expected no mutations, got %d", n)
	}

	if len(report.Trace) != 1 || !report.Trace[0].Intercepted {
		t.Fatalf("expected 1 intercepted trace step, got %+v", report.Trace)
	}
	if !strings.Contains(report.Trace[0].Note, "blocked") {
		t.Errorf("expected block note on trace step, got %q", report.Trace[0].Note)
	}
}

// TestDryRun_UnavailableWithoutRunner is documented via the entry point: with
// the runner registered (this package links it), RunBundleDryRun never returns
// the unavailable error. The negative path (no runner) is covered by the
// internal package, which does not link the bridge.
func TestDryRun_RunnerRegistered(t *testing.T) {
	eng := newDryRunEngine(t)
	_, err := memql.RunBundleDryRun(context.Background(), eng, memql.DryRunRequest{
		AutomationName:   "sandboxWebhook",
		AutomationSource: dryRunWebhookAutomation,
	})
	if err != nil {
		t.Fatalf("with the bridge linked, dry-run must be available; got %v", err)
	}
}
