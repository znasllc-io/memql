package bus

import (
	"context"
	"sync/atomic"
)

// ChannelConfig holds buffer sizes for all channels in the bus.
// Default is used when no per-channel override is specified.
// Overrides maps channel names to specific buffer sizes.
//
// Buffer sizes are intended to be tuned via telemetry data in the future.
// The telemetry component collects fill-level metrics for each channel,
// which can inform dynamic resizing.
type ChannelConfig struct {
	Default   int            // Default buffer size (64)
	Overrides map[string]int // Per-channel buffer size overrides
}

// DefaultChannelConfig returns a ChannelConfig with sensible defaults.
func DefaultChannelConfig() ChannelConfig {
	return ChannelConfig{
		Default:   64,
		Overrides: make(map[string]int),
	}
}

// BufferSize returns the buffer size for the named channel.
// If an override exists for the name, it is used; otherwise Default is returned.
func (c ChannelConfig) BufferSize(name string) int {
	if size, ok := c.Overrides[name]; ok {
		return size
	}
	return c.Default
}

// TelemetryHooks contains optional callbacks invoked by Channel operations.
// These are placeholders for the telemetry component to observe channel behavior.
type TelemetryHooks[T any] struct {
	OnSend func(name string)           // Called on successful send
	OnDrop func(name string)           // Called when a non-blocking send fails (channel full)
	OnFull func(name string, fill int) // Called when channel reaches capacity
}

// Channel is a named, instrumented wrapper around a Go channel.
// It provides non-blocking and blocking send operations, fill-level
// reporting for telemetry, and hook points for observability.
type Channel[T any] struct {
	Name string
	C    chan T

	sent    atomic.Int64
	dropped atomic.Int64
	hooks   TelemetryHooks[T]
}

// NewChannel creates a named channel with the buffer size determined by config.
func NewChannel[T any](name string, config ChannelConfig) *Channel[T] {
	return &Channel[T]{
		Name: name,
		C:    make(chan T, config.BufferSize(name)),
	}
}

// NewChannelWithHooks creates a named channel with telemetry hooks attached.
func NewChannelWithHooks[T any](name string, config ChannelConfig, hooks TelemetryHooks[T]) *Channel[T] {
	ch := NewChannel[T](name, config)
	ch.hooks = hooks
	return ch
}

// Send attempts a non-blocking send. Returns true if the message was sent,
// false if the channel is full (message dropped).
func (ch *Channel[T]) Send(msg T) bool {
	select {
	case ch.C <- msg:
		ch.sent.Add(1)
		if ch.hooks.OnSend != nil {
			ch.hooks.OnSend(ch.Name)
		}
		return true
	default:
		ch.dropped.Add(1)
		if ch.hooks.OnDrop != nil {
			ch.hooks.OnDrop(ch.Name)
		}
		return false
	}
}

// SendBlocking sends a message, blocking until space is available or ctx is done.
// Returns nil on successful send, or ctx.Err() if the context was cancelled.
func (ch *Channel[T]) SendBlocking(ctx context.Context, msg T) error {
	select {
	case ch.C <- msg:
		ch.sent.Add(1)
		if ch.hooks.OnSend != nil {
			ch.hooks.OnSend(ch.Name)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Fill returns the current number of items in the channel buffer.
func (ch *Channel[T]) Fill() int {
	return len(ch.C)
}

// Cap returns the channel buffer capacity.
func (ch *Channel[T]) Cap() int {
	return cap(ch.C)
}

// Sent returns the total number of messages successfully sent.
func (ch *Channel[T]) Sent() int64 {
	return ch.sent.Load()
}

// Dropped returns the total number of messages dropped (non-blocking send failures).
func (ch *Channel[T]) Dropped() int64 {
	return ch.dropped.Load()
}
