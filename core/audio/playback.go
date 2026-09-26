package audio

import (
	"context"
	"sync"
	"time"
)

// PlaybackGate pauses media while an apparent interruption is transcribed.
// It does not cancel generation or discard buffered samples. Every pause has
// a deadline so an unavailable transcriber cannot leave playback suspended.
type PlaybackGate struct {
	mu      sync.Mutex
	until   time.Time
	changed chan struct{}
}

func (g *PlaybackGate) signal() {
	if g.changed != nil {
		close(g.changed)
	}
	g.changed = make(chan struct{})
}
func (g *PlaybackGate) Pause(duration time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.until = time.Now().Add(duration)
	g.signal()
}
func (g *PlaybackGate) Resume() { g.mu.Lock(); defer g.mu.Unlock(); g.until = time.Time{}; g.signal() }
func (g *PlaybackGate) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		g.mu.Lock()
		remaining, changed := time.Until(g.until), g.changed
		g.mu.Unlock()
		if remaining <= 0 {
			return nil
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}

type playbackKey struct{}

func WithPlaybackGate(ctx context.Context, gate *PlaybackGate) context.Context {
	return context.WithValue(ctx, playbackKey{}, gate)
}
func WaitForPlayback(ctx context.Context) error {
	if gate, ok := ctx.Value(playbackKey{}).(*PlaybackGate); ok && gate != nil {
		return gate.Wait(ctx)
	}
	return ctx.Err()
}
