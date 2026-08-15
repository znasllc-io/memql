package memql

// constructs_handlers.go -- the gRPC adapter over the engine's construct
// catalog (memql#3749).
//
// A thin projection, deliberately. Every judgement the catalog makes -- which
// constructs exist, which kind each is, where it came from, whether it is
// runnable, what arguments it takes -- is made once in
// component/memql/construct_catalog.go and read here. This file converts
// structs; it decides nothing.
//
// Authorization mirrors the pack browser next door: none beyond the stream's
// own authentication. A construct's DEFINITION is not data, and gating it more
// tightly than the source file it came from would be incoherent.

import (
	"google.golang.org/grpc/codes"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// handleListConstructs handles ListConstructsMsg requests.
func (s *streamSession) handleListConstructs(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.ListConstructsMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "list_constructs: request body missing")
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())

	if s.service == nil || s.service.engine == nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.FailedPrecondition, "list_constructs: engine is not available on this node")
	}

	entries := s.service.engine.ConstructCatalog()
	out := make([]*memqlv1.ConstructInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, toConstructInfo(e))
	}

	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_ListConstructsResult{
			ListConstructsResult: &memqlv1.ListConstructsResult{
				RequestId:  requestId,
				Constructs: out,
			},
		},
	})
}

// toConstructInfo projects one catalog entry onto the wire.
func toConstructInfo(e memqlengine.ConstructCatalogEntry) *memqlv1.ConstructInfo {
	args := make([]*memqlv1.ConstructArg, 0, len(e.Args))
	for _, a := range e.Args {
		args = append(args, &memqlv1.ConstructArg{
			Name:         a.Name,
			Type:         a.Type,
			Required:     a.Required,
			EnumValues:   a.Enum,
			Description:  a.Description,
			AutoInjected: a.AutoInjected,
		})
	}
	// NIL STAYS NIL. Only an automation has a trigger, and an empty
	// ConstructTrigger on every other construct would read as "a trigger that
	// fires on nothing" rather than as "not applicable" -- and protojson would
	// then ship the object on all ~900 of them (memql#3805).
	var trigger *memqlv1.ConstructTrigger
	if e.Trigger != nil {
		trigger = &memqlv1.ConstructTrigger{
			Concept:  e.Trigger.Concept,
			Event:    e.Trigger.Event,
			Schedule: e.Trigger.Schedule,
		}
	}
	return &memqlv1.ConstructInfo{
		Name:         e.Name,
		Kind:         e.Kind,
		Namespace:    e.Namespace,
		Origin:       e.Origin,
		OriginPath:   e.OriginPath,
		Description:  e.Description,
		Runnable:     e.Runnable,
		Args:         args,
		BoundConcept: e.BoundConcept,
		SourceHash:   e.SourceHash,
		Source:       e.Source,
		Trigger:      trigger,
	}
}
