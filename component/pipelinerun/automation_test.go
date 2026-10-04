package pipelinerun

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
)

// automation_test.go -- what the shipped automations promise this package,
// read off the automation the loader compiles from the embedded tree rather
// than off the .memql text.

// The trigger automation retries the builtin. A delivery is staged once, and
// the automation fires once: a trigger that failed for a reason that passes
// -- above all a gate it could not take within the production gate's five
// seconds, the direct pool busy -- would lose the delivery for good. Retrying
// is safe because the trigger is idempotent through the run key: a retry
// finds every run its earlier attempt opened, and opens only what that one
// could not.
func TestTheTriggerAutomationRetriesTheTrigger(t *testing.T) {
	loader := automations.NewLoader(automations.LoaderOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	a, err := loader.LoadByName("triggerPipelinesOnGitHubDelivery")
	if err != nil || a == nil {
		t.Fatalf("the shipped trigger automation: %v %v", a, err)
	}
	var calls []*automations.Step
	for _, s := range a.Steps {
		if s.Function != nil && s.Function.Name == "pipelinesTrigger" {
			calls = append(calls, s)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("the automation calls pipelinesTrigger %d times, want once (steps %+v)", len(calls), a.Steps)
	}
	call := calls[0]
	if call.RetryCount != 3 {
		t.Errorf("the trigger's call retries %d times, want 3", call.RetryCount)
	}
	if len(call.Function.Args) != 1 || call.Function.Args["inboundRequestId"] == nil {
		t.Errorf("the trigger is handed the staged row's id and nothing else: %v", call.Function.Args)
	}
}

// A gate that could not be TAKEN says it is the pipelines' gate, and which
// key: the production gate's own error names GitHub's lifecycle lock, and a
// run that was not opened must read as a pipelines failure in the automation's
// failed step. What a section RETURNS is not the gate's failure, and passes
// through untouched -- ErrRunInProgress, a refusal -- for its caller to read.
func TestAGateThatCouldNotBeTakenSaysPipelines(t *testing.T) {
	acquire := errors.New("github: lifecycle gate connection: context deadline exceeded")
	d := Deps{Gate: func(context.Context, string, func(context.Context) error) error { return acquire }}
	ran := false
	err := d.gate(context.Background(), RepositoryGateKey(repoName), func(context.Context) error { ran = true; return nil })
	if ran {
		t.Fatalf("the section ran without its gate")
	}
	if !errors.Is(err, acquire) {
		t.Errorf("the gate's own error is kept: %v", err)
	}
	if msg := err.Error(); !strings.HasPrefix(msg, "pipelines:") || !strings.Contains(msg, RepositoryGateKey(repoName)) {
		t.Errorf("the failure names pipelines and its key: %q", msg)
	}

	held := newKeyedGate(t)
	d = Deps{Gate: held.run}
	if err := d.gate(context.Background(), OpenGateKey("k"), func(context.Context) error { return ErrRunInProgress }); err != ErrRunInProgress {
		t.Errorf("a section's own answer passes through untouched: %v", err)
	}
	d = Deps{}
	if err := d.gate(context.Background(), OpenGateKey("k"), func(context.Context) error { return nil }); !errors.Is(err, errNoGate) {
		t.Errorf("no gate wired fails closed: %v", err)
	}
}
