package bus

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultChannelConfig(t *testing.T) {
	cfg := DefaultChannelConfig()
	if cfg.Default != 64 {
		t.Errorf("expected default buffer size 64, got %d", cfg.Default)
	}
	if cfg.Overrides == nil {
		t.Error("expected non-nil overrides map")
	}
}

func TestChannelConfigBufferSize(t *testing.T) {
	cfg := ChannelConfig{
		Default:   64,
		Overrides: map[string]int{"fast": 256, "slow": 16},
	}

	if got := cfg.BufferSize("fast"); got != 256 {
		t.Errorf("expected 256 for 'fast', got %d", got)
	}
	if got := cfg.BufferSize("slow"); got != 16 {
		t.Errorf("expected 16 for 'slow', got %d", got)
	}
	if got := cfg.BufferSize("unknown"); got != 64 {
		t.Errorf("expected default 64 for 'unknown', got %d", got)
	}
}

func TestNewChannel(t *testing.T) {
	cfg := ChannelConfig{Default: 32, Overrides: map[string]int{}}
	ch := NewChannel[int]("test", cfg)

	if ch.Name != "test" {
		t.Errorf("expected name 'test', got %q", ch.Name)
	}
	if ch.Cap() != 32 {
		t.Errorf("expected cap 32, got %d", ch.Cap())
	}
	if ch.Fill() != 0 {
		t.Errorf("expected fill 0, got %d", ch.Fill())
	}
}

func TestChannelSendNonBlocking(t *testing.T) {
	cfg := ChannelConfig{Default: 2, Overrides: map[string]int{}}
	ch := NewChannel[int]("test", cfg)

	// Send within capacity
	if !ch.Send(1) {
		t.Error("expected send to succeed")
	}
	if !ch.Send(2) {
		t.Error("expected send to succeed")
	}

	// Channel is full
	if ch.Send(3) {
		t.Error("expected send to fail when channel is full")
	}

	if ch.Sent() != 2 {
		t.Errorf("expected 2 sent, got %d", ch.Sent())
	}
	if ch.Dropped() != 1 {
		t.Errorf("expected 1 dropped, got %d", ch.Dropped())
	}
	if ch.Fill() != 2 {
		t.Errorf("expected fill 2, got %d", ch.Fill())
	}
}

func TestChannelSendBlocking(t *testing.T) {
	cfg := ChannelConfig{Default: 1, Overrides: map[string]int{}}
	ch := NewChannel[int]("test", cfg)

	ctx := context.Background()

	// First send should succeed immediately
	if err := ch.SendBlocking(ctx, 1); err != nil {
		t.Errorf("expected no error, got %v", err)
	}

	// Second send should block; cancel via context
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := ch.SendBlocking(ctx, 2)
	if err == nil {
		t.Error("expected error from context timeout")
	}
}

func TestChannelSendBlockingSucceedsAfterDrain(t *testing.T) {
	cfg := ChannelConfig{Default: 1, Overrides: map[string]int{}}
	ch := NewChannel[int]("test", cfg)

	// Fill the channel
	ch.Send(1)

	// Drain in background after a short delay
	go func() {
		time.Sleep(5 * time.Millisecond)
		<-ch.C
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	if err := ch.SendBlocking(ctx, 2); err != nil {
		t.Errorf("expected send to succeed after drain, got %v", err)
	}
}

func TestChannelTelemetryHooks(t *testing.T) {
	var sendCount, dropCount atomic.Int32

	cfg := ChannelConfig{Default: 1, Overrides: map[string]int{}}
	hooks := TelemetryHooks[int]{
		OnSend: func(name string) { sendCount.Add(1) },
		OnDrop: func(name string) { dropCount.Add(1) },
	}
	ch := NewChannelWithHooks("test", cfg, hooks)

	ch.Send(1)   // succeeds
	ch.Send(2)   // drops (full)

	if sendCount.Load() != 1 {
		t.Errorf("expected 1 send hook call, got %d", sendCount.Load())
	}
	if dropCount.Load() != 1 {
		t.Errorf("expected 1 drop hook call, got %d", dropCount.Load())
	}
}
