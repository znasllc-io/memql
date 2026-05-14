package events

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/visionarys-io/memql/component/bus"
	busv1 "github.com/visionarys-io/memql/component/bus/gen"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRunWithChannelDeliversEvents(t *testing.T) {
	b := NewBus()
	defer b.Close()

	cfg := bus.DefaultChannelConfig()
	ch := bus.NewChannel[*busv1.InternalMessage]("test.events", cfg)

	var received atomic.Int32
	b.Subscribe("test.topic", func(event Event) {
		if event.Topic != "test.topic" {
			t.Errorf("expected topic 'test.topic', got %q", event.Topic)
		}
		if event.Payload["key"] != "value" {
			t.Errorf("expected payload key=value, got %v", event.Payload["key"])
		}
		received.Add(1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go b.RunWithChannel(ctx, ch)

	// Send an event via channel
	payload, _ := structpb.NewStruct(map[string]any{"key": "value"})
	msg := bus.NewMessage()
	msg.Payload = &busv1.InternalMessage_EventPublish{
		EventPublish: &busv1.EventPublish{
			Topic:     "test.topic",
			Kind:      int32(KindNodeCreated),
			Timestamp: timestamppb.Now(),
			Payload:   payload,
		},
	}

	ch.Send(msg)

	// Wait for delivery
	time.Sleep(50 * time.Millisecond)

	if received.Load() != 1 {
		t.Errorf("expected 1 event delivered, got %d", received.Load())
	}
}

func TestRunWithChannelStopsOnCancel(t *testing.T) {
	b := NewBus()
	defer b.Close()

	cfg := bus.DefaultChannelConfig()
	ch := bus.NewChannel[*busv1.InternalMessage]("test.events", cfg)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		b.RunWithChannel(ctx, ch)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// good
	case <-time.After(1 * time.Second):
		t.Error("RunWithChannel did not stop on context cancel")
	}
}

func TestHandleChannelMessageIgnoresNonEventPayload(t *testing.T) {
	b := NewBus()
	defer b.Close()

	var received atomic.Int32
	b.Subscribe("#", func(event Event) {
		received.Add(1)
	})

	// Send a non-event message (ConfigSnapshot)
	msg := bus.NewMessage()
	msg.Payload = &busv1.InternalMessage_ConfigSnapshot{
		ConfigSnapshot: &busv1.ConfigSnapshot{Version: "test"},
	}

	b.handleChannelMessage(msg)

	// Should not have delivered anything
	time.Sleep(20 * time.Millisecond)
	if received.Load() != 0 {
		t.Errorf("expected 0 events for non-event payload, got %d", received.Load())
	}
}
