package audio

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPlaybackPauseResumesAndCancellationWins(t *testing.T) {
	var gate PlaybackGate
	gate.Pause(time.Second)
	ctx, cancel := context.WithCancel(WithPlaybackGate(context.Background(), &gate))
	done := make(chan error, 1)
	go func() { done <- WaitForPlayback(ctx) }()
	select {
	case <-done:
		t.Fatal("paused frame escaped")
	case <-time.After(10 * time.Millisecond):
	}
	gate.Resume()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	gate.Pause(time.Second)
	cancel()
	if err := WaitForPlayback(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	gate.Pause(time.Millisecond)
	if err := gate.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
