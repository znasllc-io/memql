package audio

import (
	"context"
	"sync"
	"time"
)

// PlaybackGate pauses speech while an apparent interruption is transcribed.
// Media adapters keep sending silence on their normal clock without consuming
// buffered speech. Every pause expires so unavailable ASR cannot suspend it forever.
type PlaybackGate struct {
	mu    sync.Mutex
	until time.Time
}

func (g *PlaybackGate) Pause(duration time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.until = time.Now().Add(duration)
}
func (g *PlaybackGate) Resume() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.until = time.Time{}
}
func (g *PlaybackGate) paused() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.until)
}

type playbackKey struct{}

func WithPlaybackGate(ctx context.Context, gate *PlaybackGate) context.Context {
	return context.WithValue(ctx, playbackKey{}, gate)
}
func PlaybackPaused(ctx context.Context) bool {
	if gate, ok := ctx.Value(playbackKey{}).(*PlaybackGate); ok && gate != nil {
		return gate.paused()
	}
	return false
}
