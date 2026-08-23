package memql

// The fan-out half of row authorization on graph subscriptions
// (memql#4309). The DECISION lives in component/memql
// (rowauthz_subscription.go), beside the rowAuthzAdmits it defers to;
// these are the two shape adapters the decision needs and nothing more.

import (
	"encoding/json"
	"strings"

	"github.com/znasllc-io/memql/component/events"
)

// graphRowFromEvent pulls the (concept, id, stored payload) a row-authz
// decision needs out of a bus event, and reports whether the event
// describes a graph ROW at all.
//
// Only `graph.node.*` topics do. The other kinds carry node-level events
// with no row owner to decide by -- they are gated at SUBSCRIBE time
// instead (memql#4311), and running a row gate over them here would ask a
// question they cannot answer and then have to invent a default.
//
// The payload handed back is the row's own stored payload -- the nested
// `payload` object executeWrite retains alongside the flattened copy --
// because that is what the declared owner field is a TOP-LEVEL key of. The
// flattened envelope is the fallback: executeWrite merges the row's fields
// into the envelope's top level too, so an event that carries no nested
// object still resolves its owner. Falling back rather than failing closed
// is safe in the only direction that matters: a payload the gate cannot
// read an owner off DENIES (rowAuthzAdmits returns deny when the declared
// owner field is absent), so a wrong guess here withholds a row, never
// leaks one.
func graphRowFromEvent(event events.Event) (concept, id string, payload []byte, ok bool) {
	if !strings.HasPrefix(event.Topic, events.TopicGraphNode+".") {
		return "", "", nil, false
	}
	if event.Payload == nil {
		return "", "", nil, false
	}
	concept, _ = event.Payload["concept"].(string)
	id, _ = event.Payload["id"].(string)
	if strings.TrimSpace(concept) == "" {
		// No concept, no declaration to resolve, nothing this gate can say.
		// The subscribe-time kind gate is what covers events of this shape.
		return "", "", nil, false
	}

	source := event.Payload
	if nested, isMap := event.Payload["payload"].(map[string]any); isMap && nested != nil {
		source = nested
	}
	raw, err := json.Marshal(source)
	if err != nil {
		// Unreadable payload: hand back an empty one. The gate denies a row
		// whose declared owner field it cannot resolve, which is the
		// direction this has to fail in.
		return concept, id, nil, true
	}
	return concept, id, raw, true
}

// idOnlyEventPayload reduces a wire payload to the four keys an ID-ONLY
// notification carries: concept, id, action and createdAt (design D3).
//
// Built by SELECTING keys rather than by deleting the ones known to be
// sensitive. A deny-list over a payload whose shape is the row's own
// schema is a list that goes stale the moment a concept adds a field --
// and it goes stale silently, in the leaking direction. The four keys are
// what the client needs to re-read the row; everything else is the row.
//
// `topic` and `eventKind` are kept because they are the ACTION: a client
// that cannot tell a create from a delete cannot decide what to re-read.
// Neither is derived from the row's contents.
func idOnlyEventPayload(payload map[string]any) map[string]any {
	out := make(map[string]any, 5)
	for _, k := range []string{"concept", "id", "nodeId", "createdAt", "topic", "eventKind"} {
		if v, present := payload[k]; present {
			out[k] = v
		}
	}
	return out
}
