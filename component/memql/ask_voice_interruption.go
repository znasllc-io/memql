package memql

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/audio"
	"github.com/znasllc-io/memql/core/id"
)

// A short recent playback reference supplements the browser's acoustic echo
// canceller. It is not a substitute for AEC: only a multi-word transcript
// contained in recently spoken text is rejected. New words (including stop)
// remain an interruption, even when they quote part of the reply.
type voicePlayback struct {
	gate           audio.PlaybackGate
	mu             sync.Mutex
	words          []string
	since, through time.Time
}

func speechWords(text string) []string {
	return strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return unicode.ToLower(r)
		}
		return ' '
	}, text))
}
func (p *voicePlayback) speaking(text string, duration time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.words = append(p.words, speechWords(text)...)
	if len(p.words) > 160 {
		p.words = p.words[len(p.words)-160:]
	}
	now := time.Now()
	if p.since.IsZero() {
		p.since = now
	}
	p.through = now.Add(duration + 3*time.Second)
}
func (p *voicePlayback) echo(text string) bool { return p.echoAt(text, time.Now()) }

func (p *voicePlayback) echoAt(text string, heardAt time.Time) bool {
	words := speechWords(text)
	if len(words) < 3 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if heardAt.Before(p.since) || heardAt.After(p.through) {
		return false
	}
	return strings.Contains(" "+strings.Join(p.words, " ")+" ", " "+strings.Join(words, " ")+" ")
}

type voiceInput struct {
	heardAt time.Time
	id      string
	text    string
	events  []WorkEvent
	err     error
}

func (e *MemQLEngine) transcribeVoiceInput(ctx context.Context, opts AskVoiceOptions, pcm []byte) voiceInput {
	input := voiceInput{id: id.NewShortId()}
	// A bounded ASR attempt is separate from the durable work lifetime. If it
	// fails, an existing reply resumes rather than being silently discarded.
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var mu sync.Mutex
	ctx = airoute.WithObserver(ctx, func(call airoute.CallObservation) {
		mu.Lock()
		defer mu.Unlock()
		input.events = append(input.events, WorkEvent{ID: call.ID, Kind: "model", Phase: call.Phase, At: time.Now().UTC(), Provider: call.Provider, Model: call.Model, ElapsedMS: int64(call.ElapsedMS), Error: call.Error, Call: &call})
	})
	transcript, err := e.TranscribeAudio(ctx, FleetAudio{Data: audio.CreateWAVChunk(pcm, 24000, 1, 16), MediaType: "audio/wav", SampleRateHz: 24000}, opts.TranscriptionProvider)
	input.text, input.err = strings.TrimSpace(transcript.Text), err
	return input
}
