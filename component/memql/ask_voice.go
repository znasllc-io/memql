package memql

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/audio"
	"github.com/znasllc-io/memql/core/id"
)

// Installed once at bootstrap on agent nodes; live media never owns authority.
func (e *MemQLEngine) SetVoiceTransport(transport audio.RoomTransport) { e.voiceTransport = transport }

type AskVoiceOptions struct{ ConversationID, PageContext, Voice, ChatProvider, TranscriptionProvider, SpeechProvider string }
type AskVoiceEvent struct {
	Type     string     `json:"type"`
	State    string     `json:"state,omitempty"`
	TurnID   string     `json:"turnId,omitempty"`
	Text     string     `json:"text,omitempty"`
	Error    string     `json:"error,omitempty"`
	Activity *WorkEvent `json:"activity,omitempty"`
}

func (e *MemQLEngine) StartAskVoice(ctx context.Context, opts AskVoiceOptions) (audio.RoomCredentials, error) {
	subject, ok := auth.SubjectFromContext(ctx)
	if !ok || !auth.CapableFor(ctx, subject, "read", "app:ask") {
		return audio.RoomCredentials{}, fmt.Errorf("Ask is unavailable for this person")
	}
	if opts.Voice != "male" && opts.Voice != "female" {
		return audio.RoomCredentials{}, fmt.Errorf("choose a male or female voice")
	}
	if len(opts.PageContext) > 24000 {
		return audio.RoomCredentials{}, fmt.Errorf("page context is too large")
	}
	if e.voiceTransport == nil {
		return audio.RoomCredentials{}, fmt.Errorf("live voice is not configured on this cluster")
	}
	if _, err := e.askRead(ctx, opts.ConversationID); err != nil {
		return audio.RoomCredentials{}, err
	}
	// Fail before requesting the microphone when either audio door is closed.
	if _, _, err := ResolveAITyped[TranscriptionAIProvider](ctx, e, AudioResolveRequest(airoute.ModalityTranscribe, opts.TranscriptionProvider)); err != nil {
		return audio.RoomCredentials{}, fmt.Errorf("no transcription route: %w", err)
	}
	if _, _, err := ResolveAITyped[SpeechAIProvider](ctx, e, AudioResolveRequest(airoute.ModalitySpeech, opts.SpeechProvider)); err != nil {
		return audio.RoomCredentials{}, fmt.Errorf("no speech route: %w", err)
	}
	release, err := e.lockAskConversationKind(ctx, opts.ConversationID, "voice")
	if err != nil {
		return audio.RoomCredentials{}, err
	}
	// The authenticated identity survives the start envelope's mesh lifetime.
	// Room departure, media failure and the session lease cancel all model work.
	sessionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
	stopStartupCancellation := context.AfterFunc(ctx, cancel)
	defer stopStartupCancellation()
	input := make(chan []byte, 64)
	room, err := e.voiceTransport.Open(sessionCtx, audio.RoomOptions{Name: "ask-" + id.NewShortId(), Identity: subject.UserId}, func(pcm []byte) {
		select {
		case input <- pcm:
		case <-sessionCtx.Done():
		default:
			cancel()
		}
	}, cancel)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		if room != nil {
			room.Close()
		}
		cancel()
		release()
		return audio.RoomCredentials{}, err
	}
	go func() {
		defer release()
		defer cancel()
		defer room.Close()
		e.driveAskVoice(sessionCtx, room, input, opts)
	}()
	return room.Credentials(), nil
}

func (e *MemQLEngine) driveAskVoice(ctx context.Context, room audio.Room, input <-chan []byte, opts AskVoiceOptions) {
	var currentCancel context.CancelFunc
	var currentDone chan struct{}
	defer func() {
		if currentCancel != nil {
			currentCancel()
		}
		if currentDone != nil {
			<-currentDone
		}
	}()
	detector := voiceActivity{}
	for {
		select {
		case <-ctx.Done():
			return
		case pcm := <-input:
			started, utterance := detector.push(pcm)
			if started {
				if currentCancel != nil {
					currentCancel()
				}
				_ = room.Event(AskVoiceEvent{Type: "state", State: "listening"})
			}
			if len(utterance) == 0 {
				continue
			}
			if currentCancel != nil {
				currentCancel()
			}
			if currentDone != nil {
				select {
				case <-currentDone:
				case <-ctx.Done():
					return
				}
			}
			turnCtx, cancel := context.WithCancel(WithProviderOverride(ctx, opts.ChatProvider))
			currentCancel = cancel
			currentDone = make(chan struct{})
			go func(done chan struct{}, samples []byte) {
				defer close(done)
				defer cancel()
				e.askVoiceTurn(turnCtx, room, opts, samples)
			}(currentDone, utterance)
		}
	}
}
func (e *MemQLEngine) askVoiceTurn(ctx context.Context, room audio.Room, opts AskVoiceOptions, pcm []byte) {
	turnID := id.NewShortId()
	state := func(value string) {
		if ctx.Err() == nil {
			_ = room.Event(AskVoiceEvent{Type: "state", State: value, TurnID: turnID})
		}
	}
	var mu sync.Mutex
	events := []WorkEvent{}
	timingHistory := e.askTimingHistory(ctx)
	observe := func(call airoute.CallObservation) {
		event := WorkEvent{ID: call.ID, Kind: "model", Phase: call.Phase, At: time.Now().UTC(), Provider: call.Provider, Model: call.Model, ElapsedMS: int64(call.ElapsedMS), Error: call.Error, Call: &call}
		if call.Phase == "running" {
			event.ExpectedMS, event.EstimateSource = askExpectedFor(timingHistory, call)
		}
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		_ = room.Event(AskVoiceEvent{Type: "activity", TurnID: turnID, Activity: &event})
	}
	audioCtx := airoute.WithObserver(ctx, observe)
	state("transcribing")
	transcript, err := e.TranscribeAudio(audioCtx, FleetAudio{Data: audio.CreateWAVChunk(pcm, 24000, 1, 16), MediaType: "audio/wav", SampleRateHz: 24000}, opts.TranscriptionProvider)
	if ctx.Err() != nil {
		_ = room.Event(AskVoiceEvent{Type: "done", TurnID: turnID})
		return
	}
	if err != nil || strings.TrimSpace(transcript.Text) == "" {
		if err == nil {
			err = fmt.Errorf("No speech was recognized. Try speaking again.")
		}
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		mu.Lock()
		captured := append([]WorkEvent(nil), events...)
		mu.Unlock()
		saveErr := e.saveAskVoiceFailure(saveCtx, opts.ConversationID, turnID, captured, err)
		cancel()
		if saveErr != nil {
			err = fmt.Errorf("%v; conversation could not be saved: %w", err, saveErr)
		}
		if ctx.Err() == nil {
			_ = room.Event(AskVoiceEvent{Type: "error", Error: err.Error(), TurnID: turnID})
			state("listening")
		}
		_ = room.Event(AskVoiceEvent{Type: "done", TurnID: turnID})
		return
	}
	_ = room.Event(AskVoiceEvent{Type: "transcript", TurnID: turnID, Text: transcript.Text})
	state("thinking")
	// Synthesize completed sentences while the model is still generating. One
	// bounded queue preserves order, propagates backpressure and stops on barge-in.
	speech := make(chan string, 4)
	speechDone := make(chan error, 1)
	go func() {
		var speechErr error
		for sentence := range speech {
			if speechErr != nil {
				continue
			}
			spoken, callErr := e.SpeakAudio(audioCtx, sentence, opts.Voice, opts.SpeechProvider)
			if callErr != nil {
				speechErr = callErr
				continue
			}
			samples, rate, decodeErr := audio.DecodeWAV(spoken.Data)
			if decodeErr != nil {
				speechErr = decodeErr
				continue
			}
			state("speaking")
			speechErr = room.Publish(ctx, samples, rate)
		}
		speechDone <- speechErr
	}()
	pending := ""
	queue := func(sentence string) {
		select {
		case speech <- sentence:
		case <-ctx.Done():
		}
	}
	_, err = e.RunAsk(ctx, opts.ConversationID, turnID, transcript.Text, opts.PageContext, func(text string) {
		_ = room.Event(AskVoiceEvent{Type: "text", TurnID: turnID, Text: text})
		pending += text
		for {
			sentence, rest := nextSpeechSegment(pending, false)
			if sentence == "" {
				break
			}
			pending = rest
			queue(sentence)
		}
	}, func(event WorkEvent) {
		_ = room.Event(AskVoiceEvent{Type: "activity", TurnID: turnID, Activity: &event})
	})
	if err == nil && strings.TrimSpace(pending) != "" {
		queue(pending)
	}
	close(speech)
	if speechErr := <-speechDone; err == nil {
		err = speechErr
	}
	// Audio calls belong to the same durable turn even if speaking was cut off.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	mu.Lock()
	captured := append([]WorkEvent(nil), events...)
	mu.Unlock()
	if saveErr := e.appendAskActivity(saveCtx, opts.ConversationID, turnID, captured, ctx.Err() != nil); saveErr != nil && err == nil {
		err = saveErr
	}
	if err != nil && ctx.Err() == nil {
		_ = room.Event(AskVoiceEvent{Type: "error", TurnID: turnID, Error: err.Error()})
	}
	_ = room.Event(AskVoiceEvent{Type: "done", TurnID: turnID})
	if ctx.Err() == nil {
		state("listening")
	}
}
func (e *MemQLEngine) appendAskActivity(ctx context.Context, conversationID, turnID string, events []WorkEvent, interrupted bool) error {
	release, err := e.lockAskConversation(ctx, conversationID)
	if err != nil {
		return err
	}
	defer release()
	row, err := e.askRead(ctx, conversationID)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(row["transcript"])
	var transcript askTranscript
	if err = json.Unmarshal(raw, &transcript); err != nil {
		return err
	}
	for i := range transcript.Turns {
		if transcript.Turns[i].ID == turnID {
			transcript.Turns[i].Activity = append(transcript.Turns[i].Activity, events...)
			transcript.Turns[i].VoiceInterrupted = interrupted
			return e.askSave(ctx, conversationID, fmt.Sprint(row["title"]), transcript)
		}
	}
	return fmt.Errorf("voice turn is unavailable")
}
func nextSpeechSegment(text string, final bool) (string, string) {
	for i, r := range text {
		end := i + len(string(r))
		if (strings.ContainsRune(".!?\n", r) && end >= 48) || end >= 500 {
			return text[:end], text[end:]
		}
	}
	if final {
		return text, ""
	}
	return "", text
}
func (e *MemQLEngine) saveAskVoiceFailure(ctx context.Context, conversationID, turnID string, events []WorkEvent, cause error) error {
	release, err := e.lockAskConversation(ctx, conversationID)
	if err != nil {
		return err
	}
	defer release()
	row, err := e.askRead(ctx, conversationID)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(row["transcript"])
	var transcript askTranscript
	if err = json.Unmarshal(raw, &transcript); err != nil {
		return err
	}
	now := time.Now().UTC()
	transcript.Turns = append(transcript.Turns, AskTurn{ID: turnID, Prompt: "", State: "error", StartedAt: now, EndedAt: &now, Activity: events, Error: cause.Error()})
	return e.askSave(ctx, conversationID, fmt.Sprint(row["title"]), transcript)
}

// A bounded, adaptive energy detector. Microphone echo cancellation runs in the
// browser; only sustained speech interrupts a reply. Timings count samples,
// never packet arrivals, so jitter cannot prematurely commit a turn.
type voiceActivity struct {
	buffer, pre   []byte
	voiced, quiet int
	active        bool
	noise         float64
}

func (v *voiceActivity) push(pcm []byte) (started bool, utterance []byte) {
	if len(pcm) < 2 {
		return
	}
	var energy float64
	for i := 0; i+1 < len(pcm); i += 2 {
		sample := float64(int16(binary.LittleEndian.Uint16(pcm[i:]))) / 32768
		energy += sample * sample
	}
	rms := math.Sqrt(energy / float64(len(pcm)/2))
	ms := len(pcm) * 1000 / (24000 * 2)
	threshold := math.Max(0.015, v.noise*3)
	speaking := rms > threshold
	if !v.active && !speaking {
		v.noise = 0.97*v.noise + 0.03*rms
	}
	if speaking {
		v.voiced += ms
		v.quiet = 0
	} else {
		v.quiet += ms
		if !v.active {
			v.voiced = 0
		}
	}
	if !v.active {
		v.pre = append(v.pre, pcm...)
		if len(v.pre) > 24000/2 {
			v.pre = v.pre[len(v.pre)-24000/2:]
		}
		if v.voiced >= 100 {
			v.active = true
			started = true
			v.buffer = append([]byte(nil), v.pre...)
			v.pre = nil
		}
		return
	}
	v.buffer = append(v.buffer, pcm...)
	if v.quiet >= 650 || len(v.buffer) >= 24000*2*30 {
		if v.voiced >= 180 {
			utterance = v.buffer
		}
		v.buffer = nil
		v.voiced = 0
		v.quiet = 0
		v.active = false
	}
	return
}
