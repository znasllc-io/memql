package pipelinerun

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql/readiness"
	"github.com/znasllc-io/memql/component/pipelines"
)

// status_test.go -- the readiness self-report, read the way the evaluator
// reads it: through readiness.IntegrationStatus, on the payload the handler
// returns.

type stubExecutor struct{}

func (stubExecutor) Execute(context.Context, pipelines.StepRequest) (pipelines.StepResult, error) {
	return pipelines.StepResult{Status: pipelines.OutcomeSucceeded}, nil
}
func (stubExecutor) Cancel(context.Context, string) error { return nil }
func (stubExecutor) Readiness(context.Context) []pipelines.RunnerReadiness {
	return []pipelines.RunnerReadiness{{NodeID: "workbench-a", Available: true, Isolation: "passed"}}
}

type reportingExecutor struct {
	stubExecutor
	reports []pipelines.RunnerReadiness
}

func (e reportingExecutor) Readiness(context.Context) []pipelines.RunnerReadiness { return e.reports }

func TestReadinessDoesNotConfuseDispatcherRegistrationWithHealthyRunners(t *testing.T) {
	for _, reports := range [][]pipelines.RunnerReadiness{
		nil,
		{{NodeID: "a", Available: false, Isolation: "unknown"}},
		{{NodeID: "a", Available: true, Isolation: "not_proven"}},
		{{NodeID: "a", Available: true, Isolation: "expired"}},
		{{NodeID: "a", Available: true, Isolation: "passed"}, {NodeID: "b", Available: true, Isolation: "failed"}},
	} {
		h := newHarness(t)
		h.store.addPipeline(testPipeline(DeliveryWebhook))
		previous := pipelines.RegisterExecutor(reportingExecutor{reports: reports})
		report := h.integ.Status(evaluatorCtx())
		pipelines.RegisterExecutor(previous)
		if report.State != "unhealthy" {
			t.Fatalf("readiness must not pass with %+v: %+v", reports, report)
		}
	}
}

// evaluatorCtx is how readiness asks: the cluster's own owner-role actor.
func evaluatorCtx() context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "system:maintenance:moduleReadiness", Role: auth.RoleOwner, Unranked: true, Synthetic: true,
	})
}

func statusOf(t *testing.T, h *harness) (state string, touched bool, raw []byte) {
	t.Helper()
	nodes, err := h.integ.handleStatus(evaluatorCtx(), map[string]any{"probe": false}, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("one node, got %d", len(nodes))
	}
	state, touched, err = readiness.IntegrationStatus(nodes[0].Payload, IntegrationName)
	if err != nil {
		t.Fatalf("the evaluator cannot read the report: %v\n%s", err, nodes[0].Payload)
	}
	return state, touched, nodes[0].Payload
}

// The module is set up when the GitHub App is installed, a repository is
// connected and a runner is present (docs/public/operate/pipelines.md,
// Readiness); anything touched short of that is partial.
func TestTheStatusReportIsWhatReadinessReads(t *testing.T) {
	cases := []struct {
		name                   string
		app, connected, runner bool
		state                  string
		touched                bool
	}{
		{"app, a connected repository and a runner", true, true, true, "configured", true},
		{"app and runner, nothing connected", true, false, true, "needs_configuration", true},
		{"app and a connected repository, no runner", true, true, false, "needs_configuration", true},
		{"app only", true, false, false, "needs_configuration", true},
		{"runner only", false, false, true, "needs_configuration", true},
		{"nothing", false, false, false, "needs_configuration", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.github.unconfigured = !c.app
			if c.connected {
				h.store.addPipeline(testPipeline(DeliveryWebhook))
			}
			var prev pipelines.Executor
			if c.runner {
				prev = pipelines.RegisterExecutor(stubExecutor{})
			} else {
				prev = pipelines.RegisterExecutor(nil)
			}
			t.Cleanup(func() { pipelines.RegisterExecutor(prev) })

			state, touched, raw := statusOf(t, h)
			if state != c.state || touched != c.touched {
				t.Errorf("state %q touched %v; want %q and %v\n%s", state, touched, c.state, c.touched, raw)
			}
		})
	}
}

// A connected repository is any ACTIVE pipeline, whoever's: a disconnected
// one is history, and an unreadable answer is not "connected".
func TestTheStatusReportCountsOnlyAnActivePipeline(t *testing.T) {
	prev := pipelines.RegisterExecutor(stubExecutor{})
	t.Cleanup(func() { pipelines.RegisterExecutor(prev) })

	h := newHarness(t)
	gone := testPipeline(DeliveryWebhook)
	gone.Status = PipelineDisconnected
	h.store.addPipeline(gone)
	if state, _, raw := statusOf(t, h); state != "needs_configuration" || !strings.Contains(string(raw), "no repository is connected") {
		t.Errorf("a disconnected pipeline connects nothing: %q\n%s", state, raw)
	}

	other := testPipeline(DeliveryPoll)
	other.ID, other.OwnerUserID = PipelineIDFor("pkg-other"), "v1:identity:user:"+otherID
	h.store.addPipeline(other)
	if state, _, raw := statusOf(t, h); state != "configured" {
		t.Errorf("a colleague's active pipeline is a connected repository of this cluster: %q\n%s", state, raw)
	}
}

func TestTheStatusReportCarriesNoValueAndRefusesNobody(t *testing.T) {
	h := newHarness(t)
	_, _, raw := statusOf(t, h)
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if _, ok := envelope["integrations"].([]any); !ok {
		t.Errorf("the envelope is {integrations: [...]}: %s", raw)
	}
	if strings.Contains(string(raw), "ghs_") {
		t.Errorf("no token in a status report: %s", raw)
	}

	if _, err := h.integ.handleStatus(context.Background(), nil, 0); err == nil {
		t.Errorf("no caller is nobody: the report must refuse")
	}
	reader := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "u", Role: auth.RoleReader})
	if _, err := h.integ.handleStatus(reader, nil, 0); err == nil {
		t.Errorf("a reader may not read the deployment's shape")
	}
}
