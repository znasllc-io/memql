package metrics

import "github.com/prometheus/client_golang/prometheus"

// Mesh event delivery (epic memql#5338).
//
// Two counters, both written on EVERY mesh node type, because every node both
// sends and hears. They exist because the failure they measure was invisible:
// a node nobody dialed heard no broadcast for its whole life, and nothing on
// any screen, in any metric or in any log line said so.
//
// Both label sets are CLOSED -- an outcome, a transport and a result are alert
// dimensions, so each is one of the constants below and never a free string.

// The outcomes of an event at one node.
const (
	// MeshOriginated is an event this node published locally and put on the
	// mesh: one per event, however many peers it went to.
	MeshOriginated = "originated"
	// MeshHeard is an event a peer delivered that this node published on its
	// local bus: a first sighting.
	MeshHeard = "heard"
	// MeshRelayed is an event this node passed on to its other peers.
	MeshRelayed = "relayed"
	// MeshDuplicate is a copy of an event this node had already heard, dropped
	// without being published.
	MeshDuplicate = "duplicate"
	// MeshHopLimited is a copy this node did not relay because it had already
	// travelled the hop limit.
	MeshHopLimited = "hop_limited"
)

// The transports a copy can leave on, and what happened to it.
const (
	// MeshTransportDialed is a stream this node opened (WorkerDialer,
	// ParentConnector).
	MeshTransportDialed = "dialed"
	// MeshTransportAccepted is a stream a peer opened to this node.
	MeshTransportAccepted = "accepted"

	// MeshCopySent is a copy queued on the transport.
	MeshCopySent = "sent"
	// MeshCopyDropped is a copy the transport could not take: its outbox was
	// full, or the stream was closing.
	MeshCopyDropped = "dropped"
)

var meshEventsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: namespace,
	Subsystem: "mesh",
	Name:      "events_total",
	Help:      "EVERY MESH NODE TYPE writes this series. Broadcast events at this node, by outcome: originated (published here and put on the mesh), heard (delivered by a peer and published here -- a first sighting), relayed (passed on to this node's other peers), duplicate (a copy of an event already heard, dropped), hop_limited (a copy not relayed because it had travelled the hop limit). THE ONE TO ALERT ON IS heard: a mesh node whose heard rate is FLAT ZERO while its peers' is not is an island -- it is running, it is healthy, and it hears nothing the rest of the cluster does (memql#5338). Identity is the exception and reads zero by design: it sends and never hears. duplicate is normal and grows with the number of links; hop_limited should stay at zero.",
}, []string{"outcome"})

var meshCopiesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: namespace,
	Subsystem: "mesh",
	Name:      "copies_total",
	Help:      "EVERY MESH NODE TYPE writes this series. Copies of broadcast events this node put on a stream, by transport -- dialed (a stream this node opened) or accepted (a stream a peer opened to this node) -- and result: sent (queued on the stream) or dropped (the stream's outbox was full, or it was closing). ANY SUSTAINED dropped RATE means a peer is not reading fast enough and is missing events; the node's Mesh page in MemQL OS names which links it has.",
}, []string{"transport", "result"})

// MeshEvent records n events with one outcome.
func MeshEvent(outcome string, n int) {
	if n <= 0 {
		return
	}
	meshEventsTotal.WithLabelValues(outcome).Add(float64(n))
}

// MeshCopy records one copy on a transport, sent or dropped.
func MeshCopy(transport, result string) {
	meshCopiesTotal.WithLabelValues(transport, result).Inc()
}

// MeshEventValue returns the current count for one outcome, for tests.
func MeshEventValue(outcome string) float64 {
	return counterValue(meshEventsTotal.WithLabelValues(outcome))
}

// MeshCopyValue returns the current count for one (transport, result), for
// tests.
func MeshCopyValue(transport, result string) float64 {
	return counterValue(meshCopiesTotal.WithLabelValues(transport, result))
}
