package memql_test

// authoring_dryrun_fulllive_test.go -- Gate-2 increment 3 coverage: the optional
// full-sandbox-live tier, gated SEPARATELY. Under full-live, webhooks are
// re-routed to a capture sink (never the automation's real target) instead of
// being blocked; the tier refuses to run without explicit approval.
//
// External test package; reuses newDryRunEngine + dryRunWebhookAutomation from
// authoring_dryrun_test.go and links the dry-run bridge.

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"

	_ "github.com/znasllc-io/memql/component/automations/steps"
)

// TestDryRun_FullLiveGatedSeparately: requesting the full-sandbox-live tier
// without FullLiveApproved is refused -- a caller can never fall into live
// webhook delivery by defaulting the mode.
func TestDryRun_FullLiveGatedSeparately(t *testing.T) {
	eng := newDryRunEngine(t)

	_, err := memql.RunBundleDryRun(t.Context(), eng, memql.DryRunRequest{
		BundleId:         "b-live-1",
		AutomationName:   "sandboxWebhook",
		AutomationSource: dryRunWebhookAutomation,
		Mode:             memql.DryRunModeFullSandboxLive,
		// FullLiveApproved deliberately omitted.
	})
	if err == nil {
		t.Fatal("expected full-sandbox-live to be refused without approval")
	}
	if !strings.Contains(err.Error(), "gated separately") {
		t.Errorf("expected a gated-separately error, got %v", err)
	}
}

// TestDryRun_FullLiveRoutesWebhookToCaptureSink: with approval, a webhook under
// the full-live tier is recorded as delivered to the capture sink (Sink set),
// NOT blocked, and never POSTed to its real target.
func TestDryRun_FullLiveRoutesWebhookToCaptureSink(t *testing.T) {
	eng := newDryRunEngine(t)

	report, err := memql.RunBundleDryRun(t.Context(), eng, memql.DryRunRequest{
		BundleId:         "b-live-2",
		AutomationName:   "sandboxWebhook",
		AutomationSource: dryRunWebhookAutomation,
		Mode:             memql.DryRunModeFullSandboxLive,
		FullLiveApproved: true,
		CaptureSinkUrl:   "https://sink.internal/capture",
		TriggerEvent:     &memql.DryRunTriggerEvent{Topic: "graph.node.created.default.v1:authoring:bundle"},
	})
	if err != nil {
		t.Fatalf("RunBundleDryRun returned error: %v", err)
	}
	if !report.OK {
		t.Fatalf("expected full-live dry-run OK, got failure: %s", report.FailureReason)
	}
	if report.Mode != memql.DryRunModeFullSandboxLive {
		t.Errorf("expected full-sandbox-live mode echoed, got %q", report.Mode)
	}

	wh := report.SideEffectManifest.BlockedWebhooks
	if len(wh) != 1 {
		t.Fatalf("expected 1 captured webhook, got %d: %+v", len(wh), wh)
	}
	if wh[0].Sink != "https://sink.internal/capture" {
		t.Errorf("expected webhook routed to the capture sink, got sink %q", wh[0].Sink)
	}
	// The capture-sink target must NOT be the automation's real webhook URL.
	if wh[0].Url == wh[0].Sink {
		t.Errorf("real target and sink should differ (real=%q sink=%q)", wh[0].Url, wh[0].Sink)
	}
	if !strings.Contains(wh[0].Url, "example.com/hook") {
		t.Errorf("expected the real target captured for review, got %q", wh[0].Url)
	}

	// The trace marks the webhook step as intercepted + re-routed to the sink.
	if len(report.Trace) != 1 || !report.Trace[0].Intercepted {
		t.Fatalf("expected 1 intercepted trace step, got %+v", report.Trace)
	}
	if !strings.Contains(report.Trace[0].Note, "capture sink") {
		t.Errorf("expected a capture-sink note on the trace step, got %q", report.Trace[0].Note)
	}
}

// TestDryRun_FullLiveDefaultSink: full-live with approval but no capture-sink URL
// records a default sink marker rather than blocking or dispatching to the real
// target.
func TestDryRun_FullLiveDefaultSink(t *testing.T) {
	eng := newDryRunEngine(t)

	report, err := memql.RunBundleDryRun(t.Context(), eng, memql.DryRunRequest{
		BundleId:         "b-live-3",
		AutomationName:   "sandboxWebhook",
		AutomationSource: dryRunWebhookAutomation,
		Mode:             memql.DryRunModeFullSandboxLive,
		FullLiveApproved: true,
	})
	if err != nil {
		t.Fatalf("RunBundleDryRun returned error: %v", err)
	}
	wh := report.SideEffectManifest.BlockedWebhooks
	if len(wh) != 1 || wh[0].Sink == "" {
		t.Fatalf("expected a default capture sink marker, got %+v", wh)
	}
}
