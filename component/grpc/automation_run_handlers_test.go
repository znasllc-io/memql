package memql

// Tests for handleRunAutomation -- the stream landing for the automation
// invoke path (memql#3310).
//
// What the relay itself does is tested where it lives
// (component/automations/run_relay_test.go) and the cross-node hop is gated in
// test/clustere2e. What is under test HERE is the wiring between the two: the
// authorization gate, the envelope translation, and -- the property that
// matters most on a multiplexed stream -- that nothing on this path returns a
// Go error, because an error out of a stream handler tears the stream down and
// takes every other in-flight request with it.

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// runnerProbe stands in for the relay: it records the request it was handed
// and emits a canned three-frame trace.
type runnerProbe struct {
	got    automations.RunRequest
	called bool
}

func (p *runnerProbe) NodeId() string   { return "bff-1" }
func (p *runnerProbe) NodeType() string { return "bff" }

func (p *runnerProbe) Run(_ context.Context, req automations.RunRequest, sink automations.RunSink) {
	p.called = true
	p.got = req
	sink.Accepted(automations.RunAccepted{
		RunId:                 "run-1",
		Automation:            req.Automation,
		RanDeployedDefinition: true,
		DefinitionNote:        "the deployed definition ran",
		TriggerKind:           "event",
		TriggerTopic:          "graph.node.created.v1:cognition:participant",
		RequestedOnNodeId:     "bff-1",
		RequestedOnNodeType:   "bff",
	})
	sink.Step(automations.RunStep{
		RunId: "run-1", Sequence: 0, StepId: "load", Status: "success",
		DurationMs: 7, Output: map[string]any{"rows": 3},
	})
	sink.Complete(automations.RunComplete{
		RunId: "run-1", Status: "completed", DurationMs: 11, StepCount: 1,
		ExecutedOnNodeId: "cognition-1", ExecutedOnNodeType: "cognition",
	})
}

func newRunAutomationSession(t *testing.T, role auth.Role, runner AutomationRunner) (*streamSession, *captureStream) {
	t.Helper()
	cs := newCaptureStream(t)
	svc := &service{
		logger:           slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		automationRunner: runner,
	}
	s := &streamSession{
		service: svc,
		stream:  cs,
		logger:  svc.logger,
		access:  &auth.AccessContext{UserId: "u1", PrimaryEmail: "op@example.com", Role: role},
	}
	s.accessLoaded = true // short-circuit ensureAccess to the seeded access
	return s, cs
}

func runAutomationEnvelope(msg *memqlv1.RunAutomationMsg) *memqlv1.MemqlClientMessage {
	return &memqlv1.MemqlClientMessage{
		MessageId: "m1",
		Payload:   &memqlv1.MemqlClientMessage_RunAutomation{RunAutomation: msg},
	}
}

// frames extracts the AutomationRunEvent frames in send order.
func runFrames(t *testing.T, cs *captureStream) []*memqlv1.AutomationRunEvent {
	t.Helper()
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]*memqlv1.AutomationRunEvent, 0, len(cs.sent))
	for _, msg := range cs.sent {
		if evt := msg.GetAutomationRunEvent(); evt != nil {
			out = append(out, evt)
		}
	}
	return out
}

// The happy path: the request translates through, and the trace comes back as
// accepted -> step -> complete, correlated by the caller's request_id.
func TestHandleRunAutomation_StreamsTheTrace(t *testing.T) {
	probe := &runnerProbe{}
	s, cs := newRunAutomationSession(t, auth.RoleOwner, probe)

	err := s.handleRunAutomation(runAutomationEnvelope(&memqlv1.RunAutomationMsg{
		RequestId:         "r1",
		Automation:        "onParticipantCreated",
		Concept:           "v1:cognition:participant",
		TargetNodeType:    "cognition",
		TimeoutMs:         5000,
		IncludeStepOutput: true,
	}), &memqlv1.RunAutomationMsg{
		RequestId:         "r1",
		Automation:        "onParticipantCreated",
		Concept:           "v1:cognition:participant",
		TargetNodeType:    "cognition",
		TimeoutMs:         5000,
		IncludeStepOutput: true,
	})
	require.NoError(t, err, "a run must never return a Go error -- that tears down the whole stream")

	require.True(t, probe.called)
	assert.Equal(t, "onParticipantCreated", probe.got.Automation)
	assert.Equal(t, "v1:cognition:participant", probe.got.Concept)
	assert.Equal(t, "cognition", probe.got.TargetNodeType)
	assert.Equal(t, 5*time.Second, probe.got.Timeout)
	assert.True(t, probe.got.IncludeStepOutput)

	frames := runFrames(t, cs)
	require.Len(t, frames, 3, "want accepted + step + complete")

	for _, f := range frames {
		assert.Equal(t, "r1", f.GetRequestId(), "every frame carries the caller's request_id")
		assert.Equal(t, "run-1", f.GetRunId())
	}

	acc := frames[0].GetAccepted()
	require.NotNil(t, acc, "a run opens with the accepted frame")
	assert.True(t, acc.GetRanDeployedDefinition(),
		"the UI must be able to STATE that the deployed definition ran; this is the field it renders")
	assert.NotEmpty(t, acc.GetDefinitionNote())
	assert.Equal(t, "event", acc.GetTriggerKind())

	step := frames[1].GetStep()
	require.NotNil(t, step)
	assert.Equal(t, "load", step.GetStepId())
	assert.Equal(t, "success", step.GetStatus())
	assert.EqualValues(t, 7, step.GetDurationMs())
	require.NotNil(t, step.GetOutput(), "include_step_output was set")
	assert.EqualValues(t, 3, step.GetOutput().GetFields()["rows"].GetNumberValue())

	done := frames[2].GetComplete()
	require.NotNil(t, done, "a run closes with exactly one complete frame")
	assert.Equal(t, "completed", done.GetStatus())
	assert.EqualValues(t, 0, done.GetErrorCode())
	assert.Equal(t, "cognition-1", done.GetExecutedOnNodeId(),
		"the UI needs to name the node (and therefore the cluster) a run executed against")
	assert.Equal(t, "cognition", done.GetExecutedOnNodeType())
}

// A caller below owner/admin is refused -- and refused IN the reply, with the
// canonical PERMISSION_DENIED code, not by an error out of the handler.
//
// The gate matches POST /automations/trigger (memql#2937): a run needs only a
// NAME to execute an automation's whole action chain, and this surface is
// strictly more capable than that one because the caller also chooses the
// trigger payload.
func TestHandleRunAutomation_RequiresOwnerOrAdmin(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleWriter, auth.RoleReader} {
		probe := &runnerProbe{}
		s, cs := newRunAutomationSession(t, role, probe)

		err := s.handleRunAutomation(
			runAutomationEnvelope(&memqlv1.RunAutomationMsg{RequestId: "r1", Automation: "anything"}),
			&memqlv1.RunAutomationMsg{RequestId: "r1", Automation: "anything"})
		require.NoError(t, err, "a refusal must not tear down the stream")
		assert.False(t, probe.called, "role %s must never reach the runner", role)

		frames := runFrames(t, cs)
		require.Len(t, frames, 2, "even a refusal is accepted + complete")
		done := frames[1].GetComplete()
		require.NotNil(t, done)
		assert.Equal(t, "refused", done.GetStatus())
		assert.EqualValues(t, 7, done.GetErrorCode(), "PERMISSION_DENIED")
		assert.Contains(t, done.GetErrorMessage(), "owner or admin")
	}
}

// Owner and admin both get through. Asserted rather than assumed, because the
// gate is one call and a regression to owner-only would be silent for admins.
func TestHandleRunAutomation_AdminIsAdmitted(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleAdmin} {
		probe := &runnerProbe{}
		s, _ := newRunAutomationSession(t, role, probe)
		require.NoError(t, s.handleRunAutomation(
			runAutomationEnvelope(&memqlv1.RunAutomationMsg{RequestId: "r1", Automation: "x"}),
			&memqlv1.RunAutomationMsg{RequestId: "r1", Automation: "x"}))
		assert.True(t, probe.called, "role %s must reach the runner", role)
	}
}

// A node with no runner wired says so, rather than answering an empty success
// that a caller would read as "the automation ran and did nothing".
func TestHandleRunAutomation_NoRunnerWired(t *testing.T) {
	s, cs := newRunAutomationSession(t, auth.RoleOwner, nil)

	require.NoError(t, s.handleRunAutomation(
		runAutomationEnvelope(&memqlv1.RunAutomationMsg{RequestId: "r1", Automation: "x"}),
		&memqlv1.RunAutomationMsg{RequestId: "r1", Automation: "x"}))

	frames := runFrames(t, cs)
	require.Len(t, frames, 2)
	done := frames[1].GetComplete()
	require.NotNil(t, done)
	assert.Equal(t, "refused", done.GetStatus())
	assert.EqualValues(t, 13, done.GetErrorCode(), "INTERNAL")
	assert.Contains(t, done.GetErrorMessage(), "automation scheduler")
}

// The badge gate must restrict a run: an operator badge is attribution-grade,
// and a run's effects (writes, LLM spend, downstream automations) outlive the
// grant's TTL containment exactly as a deploy's do.
func TestRunAutomationIsBadgeRestricted(t *testing.T) {
	s, _ := newRunAutomationSession(t, auth.RoleOwner, &runnerProbe{})
	// Stamp a live grant directly: badgeStamped short-circuits the lazy claims
	// read, which the capture stream's bare context cannot satisfy.
	s.badgeStamped = true
	s.badgeExpiresAt = time.Now().Add(time.Hour)

	verdict := s.badgeGate(runAutomationEnvelope(&memqlv1.RunAutomationMsg{RequestId: "r1"}))
	assert.Equal(t, badgeGateRestricted, verdict,
		"a walked-away kiosk must not be able to fire an automation")
}

// The badge gate's rejection path routes by the INNER request_id, so a
// stream-keyed client dispatcher actually receives the refusal.
func TestBadgePayloadRequestIdCoversRunAutomation(t *testing.T) {
	got := badgePayloadRequestId(runAutomationEnvelope(&memqlv1.RunAutomationMsg{RequestId: "r-inner"}))
	assert.Equal(t, "r-inner", got)
}

// Step outputs go through JSON rather than structpb's Go-type switch, so an
// arbitrary automation result cannot drop the whole frame.
func TestStepOutputStruct(t *testing.T) {
	t.Run("object passes through", func(t *testing.T) {
		s := stepOutputStruct(map[string]any{"a": 1, "b": "two"})
		require.NotNil(t, s)
		assert.EqualValues(t, 1, s.GetFields()["a"].GetNumberValue())
		assert.Equal(t, "two", s.GetFields()["b"].GetStringValue())
	})
	t.Run("a non-object result is wrapped, since Struct has no bare scalar", func(t *testing.T) {
		s := stepOutputStruct([]any{1, 2, 3})
		require.NotNil(t, s)
		assert.Len(t, s.GetFields()["value"].GetListValue().GetValues(), 3)
	})
	t.Run("nil stays nil", func(t *testing.T) {
		assert.Nil(t, stepOutputStruct(nil))
	})
	t.Run("an unmarshalable result yields nil rather than losing the frame", func(t *testing.T) {
		assert.Nil(t, stepOutputStruct(make(chan int)))
	})
}
