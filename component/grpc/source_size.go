package memql

// source_size.go -- the stream's bound on DSL source a client sends.
//
// Tokenising costs memory in proportion to the tokens in a source, and the
// work after it grows faster than the source does (the measurement is in
// component/language/parser/source_size.go). This stream accepts messages up
// to 32 MiB, and the Sense handlers each run their work in a goroutine of
// their own with no concurrency bound, so an oversized source is CPU and
// memory a signed-in client gets to spend before any handler can refuse
// anything. So a payload carrying DSL source is refused HERE, in
// handleMessage, before its handler runs -- one check every payload passes
// through, reading only a length.
//
// "Carrying DSL source" is a field NAME: a Sense request's `source` and an
// authoring bundle's `sources`. The payloads are found from the descriptor, so
// a new Sense or bundle request is covered by being named like the ones before
// it, and a payload that names its DSL field anything else is not --
// TestSourceBoundCoversEverySourceCarryingPayload pins the set in both
// directions. ExecuteQueryMsg.query is lexed too and is deliberately NOT
// bounded here: a generated SDK call inlines its argument values into that
// text, so a byte bound on it would limit the caller's DATA rather than its
// source. What bounds that path is the parser's chain bound.

import (
	"google.golang.org/protobuf/reflect/protoreflect"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// dslSourceFieldNames are the names a client payload's DSL source field takes.
var dslSourceFieldNames = []protoreflect.Name{"source", "sources"}

// clientPayloadOneof is the envelope's payload oneof, resolved once.
var clientPayloadOneof = (&memqlv1.MemqlClientMessage{}).ProtoReflect().Descriptor().Oneofs().ByName("payload")

// oversizedDSLSource refuses a payload whose DSL source field is over
// langparser.MaxSourceBytes, answering the payload's own request_id beside the
// refusal so a caller waiting on that id sees it. Nil for every other payload.
func oversizedDSLSource(envelope *memqlv1.MemqlClientMessage) (requestId string, err error) {
	reflected := envelope.ProtoReflect()
	payload := reflected.WhichOneof(clientPayloadOneof)
	if payload == nil || payload.Kind() != protoreflect.MessageKind {
		return "", nil
	}
	body := reflected.Get(payload).Message()
	fields := body.Descriptor().Fields()
	for _, name := range dslSourceFieldNames {
		field := fields.ByName(name)
		if field == nil || field.Kind() != protoreflect.StringKind || field.IsList() {
			continue
		}
		if err := langparser.CheckSourceSize(body.Get(field).String()); err != nil {
			if id := fields.ByName("request_id"); id != nil && id.Kind() == protoreflect.StringKind {
				requestId = body.Get(id).String()
			}
			return requestId, err
		}
	}
	return "", nil
}
