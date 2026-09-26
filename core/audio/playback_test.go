package audio

import (
	"context"
	"testing"
	"time"
)

func TestPlaybackPauseResumesAndExpires(t *testing.T) {
	var gate PlaybackGate
	ctx := WithPlaybackGate(context.Background(), &gate)
	if PlaybackPaused(ctx) || PlaybackPaused(context.Background()) {
		t.Fatal("new media started paused")
	}
	gate.Pause(time.Minute)
	if !PlaybackPaused(ctx) {
		t.Fatal("pause did not reach the media context")
	}
	gate.Resume()
	if PlaybackPaused(ctx) {
		t.Fatal("resume did not release playback")
	}
	gate.Pause(-time.Second)
	if PlaybackPaused(ctx) {
		t.Fatal("expired pause still holds playback")
	}
}
