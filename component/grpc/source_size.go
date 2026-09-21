package memql

// source_size.go -- the stream's bounds on DSL source a client sends.
//
// Tokenising costs memory in proportion to the tokens in a source, and the
// work after it grows faster than the source does (the measurement, and the
// two bounds, are in component/language/parser/source_size.go). This stream
// accepts messages up to 32 MiB, so a payload carrying DSL source is bounded
// HERE, in handleMessage, before its handler runs -- one check every payload
// passes through, reading only a length.
//
// "Carrying DSL source" is a field NAME, and the name also says which bound
// applies: a Sense request's `source` is one file, an authoring payload's
// `sources` is a bundle of constructs and gets the larger bound. The payloads
// are found from the descriptor, so a new Sense or bundle request is covered
// by being named like the ones before it, and a payload that names its DSL
// field anything else is not -- TestSourceBoundCoversEverySourceCarryingPayload
// pins the set in both directions.
//
// ExecuteQueryMsg.query is lexed too and is deliberately NOT bounded here: a
// generated SDK call inlines its argument values into that text, so a byte
// bound on it would limit the caller's DATA rather than its source. That path
// is bounded at the PARSER instead, by the bounds on what it builds -- depth
// (nesting_bound.go) and chain length (chain_bound.go) -- which hold however
// many bytes arrive.

import (
	"google.golang.org/protobuf/reflect/protoreflect"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// dslSourceField is one name a client payload's DSL source field takes, and
// the check that name earns. Ordered, so a payload that somehow carried both
// is judged the same way on every call.
type dslSourceField struct {
	name  protoreflect.Name
	check func(string) error
}

var dslSourceFields = []dslSourceField{
	{name: "source", check: langparser.CheckSourceSize},
	{name: "sources", check: langparser.CheckBundleSize},
}

// clientPayloadOneof is the envelope's payload oneof, resolved once.
var clientPayloadOneof = (&memqlv1.MemqlClientMessage{}).ProtoReflect().Descriptor().Oneofs().ByName("payload")

// oversizedDSLSource refuses a payload whose DSL source field is over the
// bound its name earns, answering the payload's own request_id beside the
// refusal so a caller waiting on that id sees it. Nil for every other payload.
func oversizedDSLSource(envelope *memqlv1.MemqlClientMessage) (requestId string, err error) {
	reflected := envelope.ProtoReflect()
	payload := reflected.WhichOneof(clientPayloadOneof)
	if payload == nil || payload.Kind() != protoreflect.MessageKind {
		return "", nil
	}
	body := reflected.Get(payload).Message()
	fields := body.Descriptor().Fields()
	for _, carrier := range dslSourceFields {
		field := fields.ByName(carrier.name)
		if field == nil || field.Kind() != protoreflect.StringKind || field.IsList() {
			continue
		}
		if err := carrier.check(body.Get(field).String()); err != nil {
			if id := fields.ByName("request_id"); id != nil && id.Kind() == protoreflect.StringKind {
				requestId = body.Get(id).String()
			}
			return requestId, err
		}
	}
	return "", nil
}
