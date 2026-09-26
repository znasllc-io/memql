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
	driveVoice(ctx, room, input, func(ctx context.Context, pcm []byte) voiceInput { return e.transcribeVoiceInput(ctx, opts, pcm) }, func(ctx context.Context, candidate voiceInput, playback *voicePlayback) {
		e.askVoiceTurn(WithProviderOverride(ctx, opts.ChatProvider), room, opts, candidate, playback)
	})
}

func driveVoice(ctx context.Context, room audio.Room, input <-chan []byte, transcribe func(context.Context, []byte) voiceInput, reply func(context.Context, voiceInput, *voicePlayback)) {
	var currentCancel context.CancelFunc
	var currentDone chan struct{}
	var playback *voicePlayback
	checked := make(chan voiceInput, 1)
	checking := false
	var heardAt time.Time
	detector := voiceActivity{}
	defer func() {
		if currentCancel != nil {
			currentCancel()
		}
		if currentDone != nil {
			<-currentDone
		}
	}()
	type capturedFrame struct {
		pcm []byte
		at  time.Time
	}
	var buffered []capturedFrame
	bufferedBytes := 0
	overflow := false
	replyRunning := func() bool {
		if currentDone == nil {
			return false
		}
		select {
		case <-currentDone:
			return false
		default:
			return true
		}
	}
	receive := func(pcm []byte, capturedAt time.Time) {
		started, utterance := detector.push(pcm)
		if started {
			heardAt = capturedAt
			if playback != nil {
				playback.gate.Pause(30 * time.Second)
			}
		}
		if len(utterance) == 0 {
			if !detector.active && playback != nil {
				playback.gate.Resume()
			}
			return
		}
		checking = true
		if playback != nil {
			playback.gate.Pause(25 * time.Second)
		}
		if !replyRunning() {
			_ = room.Event(AskVoiceEvent{Type: "state", State: "transcribing"})
		}
		go func(samples []byte, capturedAt time.Time) {
			candidate := transcribe(ctx, samples)
			candidate.heardAt = capturedAt
			select {
			case checked <- candidate:
			case <-ctx.Done():
			}
		}(utterance, heardAt)
	}
	for {
		if ctx.Err() != nil {
			return
		}
		if !checking && len(buffered) > 0 {
			frame := buffered[0]
			buffered[0] = capturedFrame{}
			buffered = buffered[1:]
			bufferedBytes -= len(frame.pcm)
			receive(frame.pcm, frame.at)
			continue
		}
		if len(buffered) == 0 {
			overflow = false
		}
		select {
		case <-ctx.Done():
			return
		case candidate := <-checked:
			checking = false
			if candidate.err != nil || candidate.text == "" || (playback != nil && playback.echoAt(candidate.text, candidate.heardAt)) {
				if playback != nil {
					playback.gate.Resume()
				}
				if !replyRunning() {
					_ = room.Event(AskVoiceEvent{Type: "state", State: "listening"})
				}
				// A false interruption is not a user message. Keep the original turn
				// and buffered audio. ASR errors are visible, but never cancel it.
				if candidate.err != nil {
					_ = room.Event(AskVoiceEvent{Type: "error", Error: "Speech could not be recognized. Please try again."})
				}
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
			playback = &voicePlayback{}
			turnCtx, cancel := context.WithCancel(audio.WithPlaybackGate(ctx, &playback.gate))
			currentCancel = cancel
			currentDone = make(chan struct{})
			go func(done chan struct{}, playback *voicePlayback, candidate voiceInput) {
				defer close(done)
				defer cancel()
				reply(turnCtx, candidate, playback)
			}(currentDone, playback, candidate)
		case pcm, ok := <-input:
			if !ok {
				return
			}
			if checking {
				// ASR can be slower than the person. Retain bounded incoming
				// speech instead of dropping everything said during recognition.
				if bufferedBytes+len(pcm) <= 24000*2*30 {
					buffered = append(buffered, capturedFrame{pcm: append([]byte(nil), pcm...), at: time.Now()})
					bufferedBytes += len(pcm)
				} else if !overflow {
					overflow = true
					_ = room.Event(AskVoiceEvent{Type: "error", Error: "Speech is arriving faster than it can be recognized. Please pause, then repeat the last part."})
				}
			} else {
				receive(pcm, time.Now())
			}
		}
	}
}
func (e *MemQLEngine) askVoiceTurn(ctx context.Context, room audio.Room, opts AskVoiceOptions, input voiceInput, playback *voicePlayback) {
	turnID := input.id
	state := func(value string) {
		if ctx.Err() == nil {
			_ = room.Event(AskVoiceEvent{Type: "state", State: value, TurnID: turnID})
		}
	}
	var mu sync.Mutex
	events := append([]WorkEvent(nil), input.events...)
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
	_ = room.Event(AskVoiceEvent{Type: "transcript", TurnID: turnID, Text: input.text})
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
			playback.speaking(sentence, time.Duration(len(samples)/2)*time.Second/time.Duration(rate))
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
	_, err := e.RunAsk(ctx, opts.ConversationID, turnID, input.text, opts.PageContext, func(text string) {
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

// A bounded, adaptive energy detector. Microphone echo cancellation runs in the
// browser; sustained energy is only a candidate until transcription confirms it. Timings count samples,
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
