package client

import (
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// dispatcher_automation_run_test.go pins the client half of the automation
// invoke path (memql#3310) against the regression that broke it (memql#3414).
//
// RunAutomation is a STREAMING surface: one request produces many
// AutomationRunEvent frames -- accepted, then a step per step, then exactly one
// complete -- all correlated by the run's request_id rather than by the
// envelope's message_id. The caller therefore parks on RegisterStream, not on
// SendAndWait.
//
// The bug these tests exist for was entirely in streamRequestId: it did not
// know the family, so every frame fell through to the uncorrelated event
// channel and the caller waited out its deadline for a terminal frame that had
// already arrived. That failure is silent on BOTH sides -- the server logged
// nothing because it did its job, and the client logged nothing because a
// message on the event channel is not an error -- which is exactly why it
// survived a compile-and-vet review and only surfaced against a real cluster.
//
// These tests need no cluster, no token and no database: the mesh was never
// involved, which is also why the LOCAL run failed identically to the
// cross-node one.

// automationRunFrame builds one frame of a run's trace.
func automationRunFrame(requestId, runId string, event any) *memqlv1.MemqlServerMessage {
	frame := &memqlv1.AutomationRunEvent{RequestId: requestId, RunId: runId}
	switch e := event.(type) {
	case *memqlv1.AutomationRunEvent_Accepted:
		frame.Event = e
	case *memqlv1.AutomationRunEvent_Step:
		frame.Event = e
	case *memqlv1.AutomationRunEvent_Complete:
		frame.Event = e
	}
	return &memqlv1.MemqlServerMessage{
		// The server correlates each frame to the request envelope as well.
		// That must NOT be what routes it: the caller used Send (not
		// SendAndWait), so nothing is registered under the message_id, and a
		// dispatcher that relied on it would deliver nothing.
		CorrelateTo: "envelope-message-id",
		Payload: &memqlv1.MemqlServerMessage_AutomationRunEvent{
			AutomationRunEvent: frame,
		},
	}
}

// TestDispatcherRoutesAutomationRunTrace is the regression gate for memql#3414.
// Remove the AutomationRunEvent case from streamRequestId and this test fails
// with the same symptom the live cluster showed: no terminal frame, ever.
func TestDispatcherRoutesAutomationRunTrace(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	const requestId = "run-req-1"
	frames, release := d.RegisterStream(requestId)
	defer release()

	stream.recvCh <- automationRunFrame(requestId, "run-1", &memqlv1.AutomationRunEvent_Accepted{
		Accepted: &memqlv1.AutomationRunAccepted{
			Automation:            "someAutomation",
			RanDeployedDefinition: true,
			DefinitionNote:        "the deployed definition ran",
			RequestedOnNodeId:     "bff-0",
			RequestedOnNodeType:   "bff",
		},
	})
	stream.recvCh <- automationRunFrame(requestId, "run-1", &memqlv1.AutomationRunEvent_Step{
		Step: &memqlv1.AutomationRunStep{Sequence: 0, StepId: "step-a", Status: "completed"},
	})
	stream.recvCh <- automationRunFrame(requestId, "run-1", &memqlv1.AutomationRunEvent_Complete{
		Complete: &memqlv1.AutomationRunComplete{
			Status:             "refused",
			ErrorCode:          5, // NOT_FOUND
			ErrorMessage:       "automation is not registered on this node",
			ExecutedOnNodeId:   "cognition-0",
			ExecutedOnNodeType: "cognition",
		},
	})

	var accepted *memqlv1.AutomationRunAccepted
	var complete *memqlv1.AutomationRunComplete
	steps := 0

	deadline := time.After(2 * time.Second)
	for complete == nil {
		select {
		case <-deadline:
			t.Fatal("no AutomationRunComplete frame -- the trace was not routed to the run's " +
				"RegisterStream listener. This is memql#3414: a caller parks forever on a " +
				"terminal frame the server already sent.")
		case msg, ok := <-frames:
			if !ok {
				t.Fatal("the stream closed before the run completed")
			}
			evt := msg.GetAutomationRunEvent()
			if evt == nil {
				continue
			}
			switch {
			case evt.GetAccepted() != nil:
				accepted = evt.GetAccepted()
			case evt.GetStep() != nil:
				steps++
			case evt.GetComplete() != nil:
				complete = evt.GetComplete()
			}
		}
	}

	if accepted == nil {
		t.Error("every run opens with an accepted frame; none was routed")
	}
	if steps != 1 {
		t.Errorf("want 1 step frame routed, got %d", steps)
	}
	if complete.GetErrorCode() != 5 {
		t.Errorf("the terminal frame must survive routing intact; got code %d", complete.GetErrorCode())
	}
	if complete.GetExecutedOnNodeType() != "cognition" {
		t.Errorf("executing-node attribution must survive routing; got %q", complete.GetExecutedOnNodeType())
	}
}

// TestStreamRequestIdCoversAutomationRun asserts the routing key directly, so a
// failure names the one-line cause rather than the downstream timeout.
func TestStreamRequestIdCoversAutomationRun(t *testing.T) {
	msg := automationRunFrame("run-req-2", "run-2", &memqlv1.AutomationRunEvent_Complete{
		Complete: &memqlv1.AutomationRunComplete{Status: "completed"},
	})
	if got := streamRequestId(msg); got != "run-req-2" {
		t.Fatalf("streamRequestId must key an AutomationRunEvent by its request_id; got %q. "+
			"Every streaming family has to be listed there or its frames are delivered "+
			"to the event channel and no listener ever sees them (memql#3414).", got)
	}
}
