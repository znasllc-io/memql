package bus

import (
	"context"
	"log/slog"
	"sync"
	"time"

	busv1 "github.com/visionarys-io/memql/component/bus/gen"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Telemetry is a channel-based component that collects metrics from
// all other components via the TelemetryCh channel.
//
// Currently a placeholder that aggregates metrics in memory. Future
// enhancements will add export to OpenTelemetry, Prometheus, or other
// backends. The collected data can also inform dynamic buffer sizing
// for channels via the ChannelConfig.
type Telemetry struct {
	wiring *Wiring
	logger *slog.Logger

	mu               sync.RWMutex
	channelSnapshots map[string]*busv1.ChannelMetrics
	componentStats   map[string]*busv1.ComponentMetrics

	cancel context.CancelFunc
	done   chan struct{}
}

// NewTelemetry creates a telemetry collector.
func NewTelemetry(wiring *Wiring, logger *slog.Logger) *Telemetry {
	return &Telemetry{
		wiring:           wiring,
		logger:           logger,
		channelSnapshots: make(map[string]*busv1.ChannelMetrics),
		componentStats:   make(map[string]*busv1.ComponentMetrics),
	}
}

// Start begins collecting metrics from the telemetry channel
// and periodically sampling channel fill levels.
func (t *Telemetry) Start(ctx context.Context) {
	ctx, t.cancel = context.WithCancel(ctx)
	t.done = make(chan struct{})

	go t.run(ctx)
}

// Stop signals the telemetry collector to shut down and waits for completion.
func (t *Telemetry) Stop() {
	if t.cancel != nil {
		t.cancel()
	}
	if t.done != nil {
		<-t.done
	}
}

func (t *Telemetry) run(ctx context.Context) {
	defer close(t.done)

	// Sample channel fill levels periodically.
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case msg, ok := <-t.wiring.TelemetryCh.C:
			if !ok {
				return
			}
			t.handleMessage(msg)

		case <-ticker.C:
			t.sampleChannels()
		}
	}
}

func (t *Telemetry) handleMessage(msg *busv1.InternalMessage) {
	if msg == nil {
		return
	}

	switch p := msg.Payload.(type) {
	case *busv1.InternalMessage_ChannelMetrics:
		t.mu.Lock()
		t.channelSnapshots[p.ChannelMetrics.ChannelName] = p.ChannelMetrics
		t.mu.Unlock()

	case *busv1.InternalMessage_ComponentMetrics:
		t.mu.Lock()
		t.componentStats[p.ComponentMetrics.ComponentName] = p.ComponentMetrics
		t.mu.Unlock()
	}
}

// sampleChannels takes a snapshot of all channel fill levels.
func (t *Telemetry) sampleChannels() {
	now := timestamppb.Now()

	t.mu.Lock()
	defer t.mu.Unlock()

	for _, info := range t.wiring.Channels() {
		t.channelSnapshots[info.Name] = &busv1.ChannelMetrics{
			ChannelName:     info.Name,
			BufferSize:      int32(info.Cap),
			CurrentFill:     int32(info.Fill),
			MessagesSent:    info.Sent,
			MessagesDropped: info.Dropped,
			SampledAt:       now,
		}
	}
}

// ChannelSnapshot returns the latest metrics for a named channel.
func (t *Telemetry) ChannelSnapshot(name string) *busv1.ChannelMetrics {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.channelSnapshots[name]
}

// AllChannelSnapshots returns a copy of all channel metrics.
func (t *Telemetry) AllChannelSnapshots() map[string]*busv1.ChannelMetrics {
	t.mu.RLock()
	defer t.mu.RUnlock()

	result := make(map[string]*busv1.ChannelMetrics, len(t.channelSnapshots))
	for k, v := range t.channelSnapshots {
		result[k] = v
	}
	return result
}

// ComponentSnapshot returns the latest metrics for a named component.
func (t *Telemetry) ComponentSnapshot(name string) *busv1.ComponentMetrics {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.componentStats[name]
}
