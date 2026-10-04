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

func TestTheStatusReportIsWhatReadinessReads(t *testing.T) {
	cases := []struct {
		name        string
		app, runner bool
		state       string
		touched     bool
	}{
		{"app and runner", true, true, "configured", true},
		{"app only", true, false, "needs_configuration", true},
		{"runner only", false, true, "needs_configuration", true},
		{"neither", false, false, "needs_configuration", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.github.unconfigured = !c.app
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
