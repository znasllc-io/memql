package memql

import (
	"google.golang.org/grpc/codes"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/language/dslspec"
)

// DSL spec export handler (memql#2125 / A4).
//
// Returns the portable JSON form of the DSL authoring surface
// (component/language/dslspec) so the cockpit and a future web/Monaco editor
// consume the EXACT same language intelligence memQL Sense is driven from.
// Read-only: the spec is assembled from the engine's own grammar tables +
// annotation registry by dslspec.Build(); there is no mutation path.
//
// Mirrors sense_handlers.go / pack_handlers.go -- a thin gRPC adapter over a
// pure package, no engine dependency.

// handleDslSpec handles DslSpecMsg requests.
func (s *streamSession) handleDslSpec(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.DslSpecMsg) error {
	if msg == nil {
		return s.sendQueryError("", envelope.GetMessageId(), codes.InvalidArgument, "dsl_spec: request body missing")
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())

	spec := dslspec.Build()
	specJSON, err := spec.JSON()
	if err != nil {
		// JSON marshalling a pure in-memory struct should never fail; surface
		// it as an Internal error rather than a silent empty reply.
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Internal, "dsl_spec: failed to serialize spec: "+err.Error())
	}

	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_DslSpecResult{
			DslSpecResult: &memqlv1.DslSpecResult{
				RequestId: requestId,
				SpecJson:  string(specJSON),
				Version:   spec.Version,
			},
		},
	})
}
