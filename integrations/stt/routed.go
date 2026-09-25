package stt

import (
	"context"
	"fmt"
	"sync"

	"github.com/znasllc-io/memql/core/audio"
)

// RoutedProvider collects a bounded dictation and resolves its model when the
// person finishes. Local ASR and federated ASR follow the same routing policy.
// It does not manufacture interim words from a batch-only inference runtime.
type RoutedProvider struct {
	Transcribe func(context.Context, []byte) (string, error)
}

func (p *RoutedProvider) Name() string { return "router" }
func (p *RoutedProvider) StartStream(ctx context.Context, cfg StreamConfig) (StreamingSession, error) {
	if p.Transcribe == nil {
		return nil, fmt.Errorf("audio routing is unavailable")
	}
	if cfg.Format != "pcm16" || cfg.Channels != 1 || (cfg.SampleRate != 16000 && cfg.SampleRate != 24000 && cfg.SampleRate != 48000) {
		return nil, fmt.Errorf("dictation requires mono PCM16 at 16, 24 or 48 kHz")
	}
	ctx, cancel := context.WithCancel(ctx)
	return &routedSession{ctx: ctx, cancel: cancel, config: cfg, transcribe: p.Transcribe, results: make(chan TranscriptionResult, 1)}, nil
}

type routedSession struct {
	mu                sync.Mutex
	ctx               context.Context
	cancel            context.CancelFunc
	config            StreamConfig
	transcribe        func(context.Context, []byte) (string, error)
	pcm               []byte
	results           chan TranscriptionResult
	finalized, closed bool
}

func (s *routedSession) SendAudio(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalized || s.closed {
		return fmt.Errorf("dictation is closed")
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if len(data)%2 != 0 || len(s.pcm)+len(data) > s.config.SampleRate*2*120 {
		return fmt.Errorf("dictation is limited to two minutes of PCM16 audio")
	}
	s.pcm = append(s.pcm, data...)
	return nil
}
func (s *routedSession) Receive() <-chan TranscriptionResult { return s.results }
func (s *routedSession) Finalize(ctx context.Context) (*FinalTranscription, error) {
	s.mu.Lock()
	if s.finalized || s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("dictation is closed")
	}
	s.finalized = true
	pcm := s.pcm
	s.pcm = nil
	s.mu.Unlock()
	defer s.Close()
	stop := context.AfterFunc(ctx, s.cancel)
	defer stop()
	if len(pcm) == 0 {
		return nil, fmt.Errorf("no audio recorded")
	}
	text, err := s.transcribe(s.ctx, audio.CreateWAVChunk(pcm, s.config.SampleRate, 1, 16))
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if !s.closed {
		s.results <- TranscriptionResult{Text: text, IsFinal: true}
	}
	s.mu.Unlock()
	return &FinalTranscription{Text: text, DurationMS: int64(len(pcm)) * 1000 / int64(s.config.SampleRate*2)}, nil
}
func (s *routedSession) Close() error {
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.pcm = nil
		close(s.results)
	}
	return nil
}
