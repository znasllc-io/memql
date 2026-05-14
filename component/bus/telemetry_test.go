package bus

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	busv1 "github.com/visionarys-io/memql/component/bus/gen"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestTelemetryCollectsChannelMetrics(t *testing.T) {
	cfg := DefaultChannelConfig()
	w := NewWiring(cfg)

	tel := NewTelemetry(w, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	tel.Start(ctx)
	defer func() {
		cancel()
		tel.Stop()
	}()

	// Send a ChannelMetrics message
	msg := NewMessage()
	msg.Payload = &busv1.InternalMessage_ChannelMetrics{
		ChannelMetrics: &busv1.ChannelMetrics{
			ChannelName:     "test.channel",
			BufferSize:      64,
			CurrentFill:     10,
			MessagesSent:    100,
			MessagesDropped: 2,
			SampledAt:       timestamppb.Now(),
		},
	}

	w.TelemetryCh.Send(msg)

	// Give the goroutine time to process
	time.Sleep(20 * time.Millisecond)

	snap := tel.ChannelSnapshot("test.channel")
	if snap == nil {
		t.Fatal("expected channel snapshot for test.channel")
	}
	if snap.MessagesSent != 100 {
		t.Errorf("expected 100 messages sent, got %d", snap.MessagesSent)
	}
	if snap.MessagesDropped != 2 {
		t.Errorf("expected 2 messages dropped, got %d", snap.MessagesDropped)
	}
}

func TestTelemetryCollectsComponentMetrics(t *testing.T) {
	cfg := DefaultChannelConfig()
	w := NewWiring(cfg)

	tel := NewTelemetry(w, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	tel.Start(ctx)
	defer func() {
		cancel()
		tel.Stop()
	}()

	msg := NewMessage()
	msg.Payload = &busv1.InternalMessage_ComponentMetrics{
		ComponentMetrics: &busv1.ComponentMetrics{
			ComponentName: "engine",
			RequestsTotal: 500,
			ErrorsTotal:   3,
			AvgLatencyMs:  12.5,
			SampledAt:     timestamppb.Now(),
		},
	}

	w.TelemetryCh.Send(msg)
	time.Sleep(20 * time.Millisecond)

	snap := tel.ComponentSnapshot("engine")
	if snap == nil {
		t.Fatal("expected component snapshot for engine")
	}
	if snap.RequestsTotal != 500 {
		t.Errorf("expected 500 requests, got %d", snap.RequestsTotal)
	}
}

func TestTelemetrySamplesChannelFillLevels(t *testing.T) {
	cfg := DefaultChannelConfig()
	w := NewWiring(cfg)

	tel := NewTelemetry(w, testLogger())

	// Manually trigger sampling instead of waiting for ticker
	tel.sampleChannels()

	snaps := tel.AllChannelSnapshots()
	if len(snaps) == 0 {
		t.Error("expected channel snapshots after sampling")
	}

	dbSnap, ok := snaps[ChanDbRequests]
	if !ok {
		t.Error("expected snapshot for db.requests")
	}
	if dbSnap.BufferSize != int32(cfg.Default) {
		t.Errorf("expected buffer size %d, got %d", cfg.Default, dbSnap.BufferSize)
	}
}
