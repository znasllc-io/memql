package agent

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/polyphon"
)

// stubSynth is a pcmSynthesizer that returns a fixed PCM byte payload.
type stubSynth struct {
	payload string
	gotText string
}

func (s *stubSynth) Synthesize(_ context.Context, cfg polyphon.TTSConfig) (io.ReadCloser, error) {
	s.gotText = cfg.Text
	return io.NopCloser(strings.NewReader(s.payload)), nil
}

func TestPCMTTSAdapter_FramesPCM(t *testing.T) {
	// 1700 bytes -> two 640-byte frames + one 420-byte trailing frame.
	payload := strings.Repeat("x", 1700)
	stub := &stubSynth{payload: payload}
	adapter := newPCMTTSAdapter(stub, nil)

	frames, err := adapter.SynthesizePCM(context.Background(), "hello", "aura-2-thalia-en")
	require.NoError(t, err)

	var total int
	var count int
	timeout := time.After(2 * time.Second)
	for {
		select {
		case fr, ok := <-frames:
			if !ok {
				assert.Equal(t, 1700, total, "all bytes framed")
				assert.Equal(t, 3, count, "640+640+420")
				assert.Equal(t, "hello", stub.gotText)
				return
			}
			total += len(fr)
			count++
		case <-timeout:
			t.Fatal("timed out waiting for frames")
		}
	}
}

func TestPCMTTSAdapter_CancelStopsStream(t *testing.T) {
	payload := strings.Repeat("y", 64000) // many frames
	adapter := newPCMTTSAdapter(&stubSynth{payload: payload}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	frames, err := adapter.SynthesizePCM(ctx, "long", "")
	require.NoError(t, err)

	// Read one frame, then cancel; the channel must close promptly.
	<-frames
	cancel()

	drained := make(chan struct{})
	go func() {
		for range frames { //nolint:revive // draining
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not stop the frame stream")
	}
}
