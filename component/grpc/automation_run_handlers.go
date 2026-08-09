package memql

import (
	"context"
	"encoding/json"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/structpb"
)

// automation_run_handlers.go is the stream landing for the automation invoke
// path (memql#3310).
//
// Automations had no invoke path on ANY surface: an automation is registered
// against a bus subscription at load time, so the only way to exercise one was
// to cause its trigger event for real. This handler closes that, and the
// closing is deliberately thin -- everything that decides what runs, where it
// runs, and what the trace says lives in component/automations' RunRelay,
// which drives the scheduler's own Executor. This file is envelope
// translation and one authorization gate.
//
// Streaming shape. A run is not a round-trip: the trace arrives as many
// AutomationRunEvent frames correlated by request_id, opening with `accepted`
// and closing with exactly one `complete`. That mirrors QueryResultChunk and
// the transcribe-stream flow rather than the request/reply surfaces.
//
// Failure shape. Failures ride in the terminating `complete` frame's
// error_code / error_message, never as a Go error out of the handler: an
// error returned from a multiplexed stream handler tears the stream down and
// takes every other in-flight request with it. deploy_control_handlers.go
// established this convention; the only errors this file returns are stream
// SEND failures, which are fatal to the stream anyway.

// AutomationRunner is the surface the stream handler dispatches a run to.
// *automations.RunRelay satisfies it. Declared here (rather than consumed as
// the concrete type) so a handler test can drive the envelope translation
// without standing up an engine, a scheduler, and an event bus.
type AutomationRunner interface {
	// Run executes one run to completion, reporting frames to sink as they
	// happen. It must always emit exactly one Complete frame, including for a
	// run it refuses.
	Run(ctx context.Context, req automations.RunRequest, sink automations.RunSink)

	// NodeId / NodeType identify the node this runner executes on, so a
	// refusal emitted before the runner is even consulted can still say which
	// cluster and node answered.
	NodeId() string
	NodeType() string
}

// SetAutomationRunner installs the automation run relay this node dispatches
// stream-requested runs to (memql#3310). Wired by app bootstrap on every node
// that carries an automation scheduler.
//
// Nil elsewhere; the handler then answers UNIMPLEMENTED rather than pretending
// the surface exists. Mirrors SetNodeMaintenanceHandler / SetDeployControlHandler:
// it updates both the Server field (picked up by the next service construction
// in Run) and the live service reference (so a post-Run install still takes
// effect).
func (s *Server) SetAutomationRunner(r AutomationRunner) {
	if s == nil {
		return
	}
	s.automationRunner = r
	if s.serviceRef != nil && s.serviceRef.svc != nil {
		s.serviceRef.svc.automationRunner = r
	}
}

// handleRunAutomation runs one named automation with a synthesized trigger
// event and streams the step trace back.
func (s *streamSession) handleRunAutomation(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.RunAutomationMsg) error {
	requestId := msg.GetRequestId()
	correlate := envelope.GetMessageId()

	sink := &streamRunSink{session: s, requestId: requestId, correlate: correlate}

	// AUTHORIZATION. Running an automation executes its whole action chain
	// server-side: writes, LLM calls, downstream automations. It is the same
	// power the HTTP POST /automations/trigger endpoint carries, and that one
	// is owner/admin gated (memql#2937) precisely because a name is the only
	// thing an attacker needs -- no unguessable id stands between a caller and
	// the effect. This surface is strictly MORE capable, since it also lets
	// the caller choose the trigger payload, so it gets the same gate.
	//
	// The caller-chosen payload is separately contained downstream: the relay
	// dispatches through ExecuteWithClientEvent, which denies internal origin
	// to a caller-supplied payload however trusted the automation body is
	// (memql#2888). Owner/admin here, client origin there -- neither alone is
	// the whole answer.
	ac := s.ensureAccess(s.stream.Context())
	if !isOwnerOrAdmin(ac) {
		if s.logger != nil {
			role := ""
			if ac != nil {
				role = string(ac.Role)
			}
			s.logger.Warn("automation run rejected -- caller not owner/admin",
				"role", role, "automation", msg.GetAutomation(), "request_id", requestId)
		}
		return sink.refuse(automations.RunCodePermissionDenied,
			"running an automation requires a cluster owner or admin")
	}

	runner := s.service.automationRunner
	if runner == nil {
		return sink.refuse(automations.RunCodeInternal,
			"this node has no automation runner (the binary carries no automation scheduler)")
	}

	req := automations.RunRequest{
		Automation:        msg.GetAutomation(),
		Payload:           msg.GetPayload().AsMap(),
		Concept:           msg.GetConcept(),
		Topic:             msg.GetTopic(),
		TargetNodeType:    msg.GetTargetNodeType(),
		Timeout:           time.Duration(msg.GetTimeoutMs()) * time.Millisecond,
		IncludeStepOutput: msg.GetIncludeStepOutput(),
	}

	// Runs synchronously on the stream's read goroutine, which is what every
	// other handler on this stream does and what keeps the frames ordered.
	// The relay bounds the wall-clock cost itself (default 60s, hard cap
	// 300s) and refuses rather than queues when too many runs are already in
	// flight on this node, so a run cannot park the session indefinitely.
	runner.Run(s.stream.Context(), req, sink)
	return sink.sendErr
}

// streamRunSink translates relay frames into AutomationRunEvent messages.
//
// It swallows nothing: a send failure is latched onto sendErr and returned
// from the handler, because a stream that cannot be written to is dead and the
// session loop needs to know. Once latched, later frames are dropped rather
// than retried -- there is nowhere to write them.
type streamRunSink struct {
	session   *streamSession
	requestId string
	correlate string
	sendErr   error
}

func (k *streamRunSink) Accepted(a automations.RunAccepted) {
	k.send(a.RunId, &memqlv1.AutomationRunEvent_Accepted{
		Accepted: &memqlv1.AutomationRunAccepted{
			Automation:            a.Automation,
			RanDeployedDefinition: a.RanDeployedDefinition,
			DefinitionNote:        a.DefinitionNote,
			TriggerKind:           a.TriggerKind,
			TriggerTopic:          a.TriggerTopic,
			RequestedOnNodeId:     a.RequestedOnNodeId,
			RequestedOnNodeType:   a.RequestedOnNodeType,
			TargetNodeType:        a.TargetNodeType,
		},
	})
}

func (k *streamRunSink) Step(st automations.RunStep) {
	k.send(st.RunId, &memqlv1.AutomationRunEvent_Step{
		Step: &memqlv1.AutomationRunStep{
			Sequence:   st.Sequence,
			StepId:     st.StepId,
			Status:     st.Status,
			DurationMs: st.DurationMs,
			Output:     stepOutputStruct(st.Output),
			Error:      st.Error,
		},
	})
}

func (k *streamRunSink) Complete(c automations.RunComplete) {
	k.send(c.RunId, &memqlv1.AutomationRunEvent_Complete{
		Complete: &memqlv1.AutomationRunComplete{
			Status:             c.Status,
			DurationMs:         c.DurationMs,
			StepCount:          c.StepCount,
			Error:              c.Error,
			ErrorCode:          c.ErrorCode,
			ErrorMessage:       c.ErrorMessage,
			ExecutedOnNodeId:   c.ExecutedOnNodeId,
			ExecutedOnNodeType: c.ExecutedOnNodeType,
		},
	})
}

// refuse emits the accepted/complete pair for a run the handler turned away
// before the relay was consulted, so a client's state machine stays uniform:
// every run opens with accepted and closes with complete.
func (k *streamRunSink) refuse(code int32, message string) error {
	k.Accepted(automations.RunAccepted{
		RanDeployedDefinition: true,
	})
	k.Complete(automations.RunComplete{
		Status:       "refused",
		ErrorCode:    code,
		ErrorMessage: message,
	})
	return k.sendErr
}

func (k *streamRunSink) send(runId string, event any) {
	if k.sendErr != nil {
		return
	}
	frame := &memqlv1.AutomationRunEvent{
		RequestId: k.requestId,
		RunId:     runId,
	}
	switch e := event.(type) {
	case *memqlv1.AutomationRunEvent_Accepted:
		frame.Event = e
	case *memqlv1.AutomationRunEvent_Step:
		frame.Event = e
	case *memqlv1.AutomationRunEvent_Complete:
		frame.Event = e
	}
	k.sendErr = k.session.sendServerMessage(k.correlate, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_AutomationRunEvent{
			AutomationRunEvent: frame,
		},
	})
}

// stepOutputStruct renders a step's result as a Struct.
//
// A step's result is arbitrary automation data -- a row map, a slice of rows,
// a scalar from a builtin -- so it goes through JSON rather than through
// structpb's Go-type switch, which rejects anything it does not recognise and
// would drop the whole output for one unexpected field. A non-object result
// is wrapped under "value", because Struct has no representation for a bare
// scalar or array. An output that will not marshal at all yields nil: the
// timeline row still arrives with its name, status and duration, which is the
// part that matters.
func stepOutputStruct(out any) *structpb.Struct {
	if out == nil {
		return nil
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err == nil {
		s, convErr := structpb.NewStruct(asMap)
		if convErr != nil {
			return nil
		}
		return s
	}
	var asAny any
	if err := json.Unmarshal(raw, &asAny); err != nil {
		return nil
	}
	s, err := structpb.NewStruct(map[string]any{"value": asAny})
	if err != nil {
		return nil
	}
	return s
}
