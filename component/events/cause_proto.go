package events

// cause_proto.go converts Cause to and from its wire form on
// component/bus/bus.proto's EventPublish (component.bus's EventCause /
// EventCauseLink messages, epic memql#5380).
//
// It lives here, not in component/node, because the EventPublish hop has two
// independent sides that must agree on the same shape: component/node's
// publishViaBus (the producer, in eventbridge_bus.go) and
// handleChannelMessage below (the consumer). A second, independently-written
// mapping in component/node is exactly how the two would drift. It converts
// only the bus.proto shape -- it does not and must not import component/node's
// generated package (component/node/gen); node.proto's EventForward carries
// the structurally-identical but distinct EventCause type, and its converter
// lives beside ITS producer and consumer in component/node/eventbridge.go.

import (
	busv1 "github.com/znasllc-io/memql/component/bus/gen"
)

// CauseToBusProto converts a Cause to the EventPublish wire form. The zero
// cause -- a root event, the common case -- converts to nil, so a root event
// costs nothing extra on the channel; CauseFromBusProto(nil) reads back as
// the zero cause.
func CauseToBusProto(c Cause) *busv1.EventCause {
	if c.IsZero() {
		return nil
	}
	chain := make([]*busv1.EventCauseLink, len(c.Chain))
	for i, l := range c.Chain {
		chain[i] = &busv1.EventCauseLink{Automation: l.Automation, RunId: l.RunId}
	}
	return &busv1.EventCause{
		CausationId:   c.CausationId,
		CorrelationId: c.CorrelationId,
		Depth:         int32(c.Depth),
		Chain:         chain,
	}
}

// CauseFromBusProto reverses CauseToBusProto. A nil proto -- an old node in a
// mixed-version rollout, or a root event -- gives the zero cause.
func CauseFromBusProto(p *busv1.EventCause) Cause {
	if p == nil {
		return Cause{}
	}
	chain := make([]Link, len(p.Chain))
	for i, l := range p.Chain {
		chain[i] = Link{Automation: l.Automation, RunId: l.RunId}
	}
	return Cause{
		CausationId:   p.CausationId,
		CorrelationId: p.CorrelationId,
		Depth:         int(p.Depth),
		Chain:         chain,
	}
}
