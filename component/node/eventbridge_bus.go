package node

import (
	"github.com/znasllc-io/memql/component/bus"
	busv1 "github.com/znasllc-io/memql/component/bus/gen"
	"github.com/znasllc-io/memql/component/events"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SetWiring configures the bus wiring for channel-based communication.
// When set, inbound events from peers are published via the EventPublishCh
// channel instead of calling localBus.Publish() directly.
func (eb *EventBridge) SetWiring(w *bus.Wiring) {
	eb.wiring = w
}

// publishViaBus sends an event through the bus EventPublishCh channel.
// Falls back to direct localBus.Publish() if wiring is not configured.
func (eb *EventBridge) publishViaBus(event events.Event) {
	if eb.wiring == nil {
		eb.localBus.Publish(event)
		return
	}

	payload, err := structpb.NewStruct(event.Payload)
	if err != nil {
		// Fall back to direct publish if payload conversion fails
		eb.localBus.Publish(event)
		return
	}

	msg := bus.NewMessage()
	msg.Payload = &busv1.InternalMessage_EventPublish{
		EventPublish: &busv1.EventPublish{
			Topic:         event.Topic,
			Kind:          int32(event.Kind),
			Timestamp:     timestamppb.New(event.Timestamp),
			Payload:       payload,
			EventMetadata: event.Metadata,
			OriginNodeId:  event.OriginNodeId,
		},
	}

	// Non-blocking send; if channel is full, fall back to direct publish
	if !eb.wiring.EventPublishCh.Send(msg) {
		eb.localBus.Publish(event)
	}
}
