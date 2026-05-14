package events

import (
	"context"
	"time"

	"github.com/visionarys-io/memql/component/bus"
	busv1 "github.com/visionarys-io/memql/component/bus/gen"
)

// RunWithChannel reads EventPublish messages from the bus channel and
// delivers them through the existing Publish() mechanism. This bridges
// channel-based components into the event bus without changing the
// internal delivery engine (goroutine-per-subscriber with panic recovery).
//
// Runs until ctx is cancelled or the channel is closed.
func (b *Bus) RunWithChannel(ctx context.Context, ch *bus.Channel[*busv1.InternalMessage]) {
	if b == nil || ch == nil {
		return
	}

	if b.logger != nil {
		b.logger.Info("event bus channel adapter started")
	}

	for {
		select {
		case <-ctx.Done():
			if b.logger != nil {
				b.logger.Info("event bus channel adapter stopped")
			}
			return
		case msg, ok := <-ch.C:
			if !ok {
				if b.logger != nil {
					b.logger.Info("event bus channel closed")
				}
				return
			}
			b.handleChannelMessage(msg)
		}
	}
}

// handleChannelMessage converts an InternalMessage with EventPublish payload
// into an events.Event and publishes it through the existing bus.
func (b *Bus) handleChannelMessage(msg *busv1.InternalMessage) {
	if msg == nil {
		return
	}

	ep, ok := msg.Payload.(*busv1.InternalMessage_EventPublish)
	if !ok || ep.EventPublish == nil {
		return
	}

	pub := ep.EventPublish

	// Convert protobuf payload to map[string]any
	var payload map[string]any
	if pub.Payload != nil {
		payload = pub.Payload.AsMap()
	}

	// Convert protobuf timestamp to time.Time
	ts := time.Now()
	if pub.Timestamp != nil {
		ts = pub.Timestamp.AsTime()
	}

	event := Event{
		Topic:        pub.Topic,
		Kind:         Kind(pub.Kind),
		Timestamp:    ts,
		Payload:      payload,
		Metadata:     pub.EventMetadata,
		OriginNodeId: pub.OriginNodeId,
	}

	b.Publish(event)
}
