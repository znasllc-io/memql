package node

import (
	"sort"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/metrics"
	"github.com/znasllc-io/memql/core/logger"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// WHAT A ROW EVENT MAY WEIGH ON THE MESH.
//
// A broadcast is not one message. Every node relays a first sighting to every
// other peer, so in a mesh of sixteen replicas each one hears about seven copies
// of each event, and it decodes all of them before dedup can throw six away.
// Decoding a Struct costs about seven times its wire size in heap. A row event
// also carries the row twice (flat, and under `payload`), and an update publishes
// two of them (`created` and `updated`).
//
// So the cost of writing a large row lands on every replica at once, whatever
// that replica does for a living. On 2026-11-01 a workflow step bound a 308 KB
// vendor answer. The journal recorded it twice in the step row (`result`, and
// `value`, which a resume rebinds), which made a 1.3 MB event, twice, for each of
// about ten steps. One such run, measured in production: 1.7 GiB of extra
// allocation in a minute on an idle mcp replica and 2.4 GiB on a planner, 40
// seconds with both cores of a 2-CPU node pegged, and up to 1.7 GiB less available
// memory on the node. Three more within three minutes took down the two nodes
// hosting thirteen of the sixteen replicas, and the first victim named was an mcp
// replica nobody had called.
//
// The row is not the problem and is not touched: the store keeps it whole, and
// the writer's own bus delivers it whole. What is bounded is what crosses the
// mesh. An event over meshMaxEventBytes leaves without its largest fields, and
// says so (events.PayloadOmittedFieldsKey). Each reader then does what it does
// for an id-only event:
//
//   - A live view gets an id-only notification and reads the row again
//     (component/grpc handleBusEvent), so it costs one read per view actually
//     open, not a decode on every replica.
//   - An automation does not fire on a thinned copy (component/automations
//     subscribeToEventTrigger). Its dedup key hashes the event's payload, so a
//     thinned copy and the whole one would never dedup and it would run twice.
//     The writer's node fires it from the whole row. KNOWN LIMIT: an automation
//     loaded only on OTHER node types does not fire for a row this large. The
//     warning below names every row it could affect.
//   - A Go subscriber reads the small fields it decides on (status, ids), which
//     stay: largest first means they are the last to go.
//
// Only a BROADCAST rule's ROW event is thinned. A non-graph topic has no row a
// reader can fetch instead, and a targeted rule reaches one node type with no
// flooding to multiply it.
const (
	// meshMaxEventBytes is the most a broadcast row event may weigh on the wire.
	// Ordinary events here average under 20 KiB (measured on production,
	// 2026-11-01: 1.1 MiB/s received at 58 copies/s on an idle replica), so this
	// is far above the traffic the mesh is for. At the limit one event costs a
	// replica about 10 MiB of allocation; the incident's cost about 100 MiB.
	meshMaxEventBytes = 128 << 10

	// meshOmittedNamesMax bounds the marker: a row of ten thousand small fields
	// must not spend its budget listing them. The names are for a person
	// reading a log; presence of the marker is what code decides on.
	meshOmittedNamesMax = 16

	// meshOmittedMarkerBudget is held back from the limit so that adding the
	// marker cannot push a copy back over it.
	meshOmittedMarkerBudget = 2 << 10

	// meshThinLogEvery rate-limits the warning, as meshDropLogEvery does.
	meshThinLogEvery = 10 * time.Second
)

// meshIdentityFields are what a reader needs to find the row and to judge the
// event, so they always travel. `payload` is the nested copy, thinned in place.
var meshIdentityFields = map[string]struct{}{
	"id":        {},
	"nodeId":    {},
	"concept":   {},
	"nodeType":  {},
	"createdAt": {},
	"topic":     {},
	"eventKind": {},
	"actor":     {},
	"payload":   {},
}

// thinOversizedRowEvent removes a row event's largest fields, flat and under
// `payload` alike, until the event fits the limit, and marks what it removed. It
// reports the event's size before and the fields left out; a row event that
// already fits comes back untouched with nil.
//
// payload is the copy built for the mesh (structpb.NewStruct made a new tree),
// so editing it cannot reach the maps the writer's own subscribers hold.
func thinOversizedRowEvent(payload *structpb.Struct) (size int, omitted []string) {
	size = proto.Size(payload)
	if size <= meshMaxEventBytes {
		return size, nil
	}

	fields := payload.GetFields()
	nested := fields["payload"].GetStructValue().GetFields()

	weights := make(map[string]int, len(fields))
	weigh := func(from map[string]*structpb.Value) {
		for name, v := range from {
			if _, identity := meshIdentityFields[name]; !identity {
				weights[name] += proto.Size(v)
			}
		}
	}
	weigh(fields)
	weigh(nested)

	names := make([]string, 0, len(weights))
	for name := range weights {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if weights[names[i]] != weights[names[j]] {
			return weights[names[i]] > weights[names[j]]
		}
		return names[i] < names[j]
	})

	// weights count values and not their map entries, so what a removal saves is
	// at least its weight and `remaining` never reads below the event's real
	// size: when it says the event fits, it does. Measuring the event after each
	// removal would be exact and quadratic, on the node that wrote the row.
	remaining := size
	for _, name := range names {
		delete(fields, name)
		delete(nested, name)
		omitted = append(omitted, name)
		remaining -= weights[name]
		if remaining <= meshMaxEventBytes-meshOmittedMarkerBudget {
			break
		}
	}

	listed := omitted
	if len(listed) > meshOmittedNamesMax {
		listed = listed[:meshOmittedNamesMax]
	}
	marker := make([]any, len(listed))
	for i, name := range listed {
		marker[i] = name
	}
	list, err := structpb.NewList(marker)
	if err != nil {
		// A field name that is not valid UTF-8 cannot be listed. The marker's
		// presence is what readers decide on, so it goes out empty.
		list = &structpb.ListValue{}
	}
	fields[events.PayloadOmittedFieldsKey] = structpb.NewListValue(list)
	return size, omitted
}

// noteThinned counts a thinned event and says so at most once per
// meshThinLogEvery. This line is how the next large row gets named: nothing else
// on this node records what a write weighed.
func (eb *EventBridge) noteThinned(event events.Event, size int, omitted []string) {
	metrics.MeshEvent(metrics.MeshThinned, 1)
	n := eb.thinned.since.Add(1)
	now := time.Now().UnixNano()
	last := eb.thinned.lastLog.Load()
	if now-last < int64(meshThinLogEvery) || !eb.thinned.lastLog.CompareAndSwap(last, now) {
		return
	}
	eb.thinned.since.Add(-n)
	concept, _ := event.Payload["concept"].(string)
	id, _ := event.Payload["id"].(string)
	listed := omitted
	if len(listed) > meshOmittedNamesMax {
		listed = listed[:meshOmittedNamesMax]
	}
	eb.logger.Warn("mesh: a row too large to broadcast went out without its largest fields; the row itself is whole",
		logger.Subject(concept, id), "bytes", size, "limit", meshMaxEventBytes,
		"omitted", listed, "thinned", n)
}

// meshThinStats rate-limits noteThinned's warning.
type meshThinStats struct {
	since   atomic.Int64
	lastLog atomic.Int64 // unix nanos
}
