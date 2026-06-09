package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/polyphon"
	"github.com/znasllc-io/memql/integrations/openai"
)

// fakeRealtimeSession is an in-memory realtimeSession. It records every client
// control call (the conductor-gate + multi-party wire) and exposes
// caller-driven audio/event channels so a test can simulate the model.
type fakeRealtimeSession struct {
	mu sync.Mutex

	responses       []string // CreateResponse instructions, in order
	cancels         int      // CancelResponse calls
	commits         int      // CommitInput calls
	injected        []openai.ConversationItem
	audioIn         [][]byte
	functionResults []fakeFunctionResult

	audioOut chan []byte
	events   chan openai.RealtimeServerEvent
	closed   bool
}

type fakeFunctionResult struct {
	callID string
	output string
}

func newFakeRealtimeSession() *fakeRealtimeSession {
	return &fakeRealtimeSession{
		audioOut: make(chan []byte, 16),
		events:   make(chan openai.RealtimeServerEvent, 16),
	}
}

func (f *fakeRealtimeSession) SendAudio(pcm16k []byte) error {
	f.mu.Lock()
	cp := append([]byte(nil), pcm16k...)
	f.audioIn = append(f.audioIn, cp)
	f.mu.Unlock()
	return nil
}

func (f *fakeRealtimeSession) CommitInput() error {
	f.mu.Lock()
	f.commits++
	f.mu.Unlock()
	return nil
}

func (f *fakeRealtimeSession) InjectItem(item openai.ConversationItem) error {
	f.mu.Lock()
	f.injected = append(f.injected, item)
	f.mu.Unlock()
	return nil
}

func (f *fakeRealtimeSession) CreateResponse(instructions string) error {
	f.mu.Lock()
	f.responses = append(f.responses, instructions)
	f.mu.Unlock()
	return nil
}

func (f *fakeRealtimeSession) CancelResponse() error {
	f.mu.Lock()
	f.cancels++
	f.mu.Unlock()
	return nil
}

func (f *fakeRealtimeSession) SendFunctionResult(callID, output string) error {
	f.mu.Lock()
	f.functionResults = append(f.functionResults, fakeFunctionResult{callID: callID, output: output})
	f.mu.Unlock()
	return nil
}

func (f *fakeRealtimeSession) AudioOut() <-chan []byte                   { return f.audioOut }
func (f *fakeRealtimeSession) Events() <-chan openai.RealtimeServerEvent { return f.events }

func (f *fakeRealtimeSession) Close() error {
	f.mu.Lock()
	if !f.closed {
		f.closed = true
		close(f.audioOut)
		close(f.events)
	}
	f.mu.Unlock()
	return nil
}

func (f *fakeRealtimeSession) responseCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.responses)
}

func (f *fakeRealtimeSession) lastResponse() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.responses) == 0 {
		return ""
	}
	return f.responses[len(f.responses)-1]
}

func (f *fakeRealtimeSession) cancelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancels
}

func (f *fakeRealtimeSession) injectedItems() []openai.ConversationItem {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]openai.ConversationItem, len(f.injected))
	copy(out, f.injected)
	return out
}

func (f *fakeRealtimeSession) audioInCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.audioIn)
}

// newRealtimeExecutorForTest wires an executor against a fakeStream-backed
// client and a fakeRealtimeSession.
func newRealtimeExecutorForTest(t *testing.T, fs *fakeStream, rt *fakeRealtimeSession) *RealtimeExecutor {
	t.Helper()
	c := newTestClient(t, fs)
	sink := &recordingSink{}
	e := NewRealtimeExecutor(context.Background(), CascadeConfig{
		SpaceID:   "s1",
		GaAgentID: "s1-ga",
		Thread:    memqlv1.VoiceAgentTurnRequest_THREAD_CONTEXT_TEAM,
	}, c, rt, sink, SessionPersona{Instructions: "be helpful", Voice: "marin"}, nil)
	t.Cleanup(e.Close)
	e.Start()
	return e
}

// replyToTurn makes the fake server reply to a VoiceAgentTurnRequest with the
// given final text (empty = conductor suppression).
func replyToTurn(fs *fakeStream, finalText string) {
	fs.onSend = func(env *memqlv1.MemqlClientMessage) {
		if env.GetVoiceAgentTurnRequest() == nil {
			return
		}
		fs.push(&memqlv1.MemqlServerMessage{
			CorrelateTo: env.GetMessageId(),
			Payload: &memqlv1.MemqlServerMessage_VoiceAgentTurnComplete{
				VoiceAgentTurnComplete: &memqlv1.VoiceAgentTurnComplete{
					RequestId: "r1", FinalText: finalText, UtteranceId: "u1",
				},
			},
		})
	}
}

// TestRealtimeExecutor_ConductorGate_Engage verifies the conductor gate engage
// path: a human final -> VoiceAgentTurnRequest -> non-empty TurnComplete ->
// exactly one response.create with a per-response directive derived from the
// reply, preceded by an input commit.
func TestRealtimeExecutor_ConductorGate_Engage(t *testing.T) {
	fs := newFakeStream()
	replyToTurn(fs, "Sure, here's the plan.")
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorForTest(t, fs, rt)

	e.handleASRResult("user-1", polyphon.ASRResult{Text: "what's the plan", IsFinal: true})

	require.Eventually(t, func() bool {
		return rt.responseCount() == 1
	}, 2*time.Second, 10*time.Millisecond, "engage must drive exactly one response.create")

	assert.Contains(t, rt.lastResponse(), "Sure, here's the plan.",
		"per-response instructions carry the conductor's decided content")
	rt.mu.Lock()
	commits := rt.commits
	rt.mu.Unlock()
	assert.GreaterOrEqual(t, commits, 1, "input buffer committed before the response")
	assert.Equal(t, StateAssistantTurn, e.Machine().State())
}

// TestRealtimeExecutor_ConductorGate_Suppress verifies the suppress path: an
// empty TurnComplete (conductor silence / classifier ack) emits NO
// response.create -- the model never self-triggers.
func TestRealtimeExecutor_ConductorGate_Suppress(t *testing.T) {
	fs := newFakeStream()
	replyToTurn(fs, "") // suppressed
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorForTest(t, fs, rt)

	e.handleASRResult("user-1", polyphon.ASRResult{Text: "mm hmm", IsFinal: true})

	// Wait until the turn request was sent + processed.
	require.Eventually(t, func() bool {
		for _, env := range fs.sentEnvelopes() {
			if env.GetVoiceAgentTurnRequest() != nil {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	assert.Equal(t, 0, rt.responseCount(), "suppressed turn must not call response.create")
	assert.Equal(t, StateListening, e.Machine().State())
}

// TestRealtimeExecutor_BargeIn_CancelsResponse verifies that a human onset
// during an in-flight response issues response.cancel + flushes the sink.
func TestRealtimeExecutor_BargeIn_CancelsResponse(t *testing.T) {
	fs := newFakeStream()
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorForTest(t, fs, rt)

	// Drive an engage directly via the SpeakSink seam, then mark the response
	// in-flight as the model would via response.created.
	require.True(t, e.Speak(SpeakDirective{Text: "a long answer", RequestID: "r9"}))
	require.Eventually(t, func() bool { return rt.responseCount() == 1 }, 2*time.Second, 10*time.Millisecond)
	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventResponseCreated, ResponseID: "resp_1"}
	require.Eventually(t, func() bool { return e.isInFlight() }, 2*time.Second, 10*time.Millisecond)

	// Human onset mid-response -> barge-in -> cancel.
	e.handleASRResult("user-1", polyphon.ASRResult{Kind: polyphon.ASRKindSpeechStarted})
	assert.Equal(t, StateHumanTurn, e.Machine().State())
	require.Eventually(t, func() bool {
		return rt.cancelCount() >= 1
	}, 2*time.Second, 10*time.Millisecond, "barge-in must issue response.cancel")
}

// TestRealtimeExecutor_MultiParty_LabeledInjection verifies per-speaker
// attribution: each human final is injected as a labeled "[name · role]"
// conversation.item before the turn commits (#433).
func TestRealtimeExecutor_MultiParty_LabeledInjection(t *testing.T) {
	fs := newFakeStream()
	replyToTurn(fs, "") // suppress so we isolate the injection
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorForTest(t, fs, rt)

	e.SetParticipant("user-maria", "Maria Lopez", "Finance Lead")
	e.SetParticipant("user-dev", "Dev Patel", "")

	e.handleASRResult("user-maria", polyphon.ASRResult{Text: "cut cloud spend", IsFinal: true})
	e.handleASRResult("user-dev", polyphon.ASRResult{Text: "migrate the database", IsFinal: true})

	require.Eventually(t, func() bool {
		return len(rt.injectedItems()) == 2
	}, 2*time.Second, 10*time.Millisecond)

	items := rt.injectedItems()
	assert.Equal(t, "user", items[0].Role)
	assert.Equal(t, "[Maria Lopez · Finance Lead] cut cloud spend", items[0].Text)
	// Dev has no role -> label is just the name.
	assert.Equal(t, "[Dev Patel] migrate the database", items[1].Text)
}

// TestRealtimeExecutor_ActiveSpeakerAudio verifies that streamed active-speaker
// PCM reaches the model's input buffer (the prosody/barge-in hear side, #433).
func TestRealtimeExecutor_ActiveSpeakerAudio(t *testing.T) {
	fs := newFakeStream()
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorForTest(t, fs, rt)

	e.StreamAudio([]byte{1, 2, 3, 4})
	e.StreamAudio([]byte{5, 6})
	e.StreamAudio(nil) // empty is a no-op

	assert.Equal(t, 2, rt.audioInCount())
}

// TestRealtimeExecutor_OutputAudio_PublishedToSink verifies the model's output
// audio frames are pumped to the room sink, and response.done collapses the
// turn back to listening.
func TestRealtimeExecutor_OutputAudio_PublishedToSink(t *testing.T) {
	fs := newFakeStream()
	rt := newFakeRealtimeSession()
	sink := &recordingSink{}
	c := newTestClient(t, fs)
	e := NewRealtimeExecutor(context.Background(), CascadeConfig{SpaceID: "s1", GaAgentID: "s1-ga"},
		c, rt, sink, SessionPersona{}, nil)
	t.Cleanup(e.Close)
	e.Start()

	require.True(t, e.Speak(SpeakDirective{Text: "hello"}))
	require.Eventually(t, func() bool { return rt.responseCount() == 1 }, 2*time.Second, 10*time.Millisecond)
	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventResponseCreated, ResponseID: "resp_1"}

	// Model streams two output frames.
	rt.audioOut <- []byte{0x11, 0x22}
	rt.audioOut <- []byte{0x33, 0x44}
	require.Eventually(t, func() bool { return sink.count() == 2 }, 2*time.Second, 10*time.Millisecond)

	// response.done collapses the turn.
	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventResponseDone, ResponseID: "resp_1"}
	require.Eventually(t, func() bool {
		return e.Machine().State() == StateListening && !e.isInFlight()
	}, 2*time.Second, 10*time.Millisecond, "response.done collapses assistant-turn to listening")
}

// TestRealtimeExecutor_OutputCapture_ForwardsTranscript verifies the #458
// output-capture seam: an EventTranscriptDone drives a VoiceAgentRealtimeOutput
// to memQL (so chat/canvas render the realtime turn).
func TestRealtimeExecutor_OutputCapture_ForwardsTranscript(t *testing.T) {
	fs := newFakeStream()
	var outputs int32
	fs.onSend = func(env *memqlv1.MemqlClientMessage) {
		if env.GetVoiceAgentRealtimeOutput() == nil {
			return
		}
		atomic.AddInt32(&outputs, 1)
		fs.push(&memqlv1.MemqlServerMessage{
			CorrelateTo: env.GetMessageId(),
			Payload: &memqlv1.MemqlServerMessage_VoiceAgentRealtimeOutputAck{
				VoiceAgentRealtimeOutputAck: &memqlv1.VoiceAgentRealtimeOutputAck{Success: true, UtteranceId: "utt-x"},
			},
		})
	}
	c := newTestClient(t, fs)
	rt := newFakeRealtimeSession()
	e := NewRealtimeExecutor(context.Background(), CascadeConfig{SpaceID: "s1", GaAgentID: "s1-ga"},
		c, rt, &recordingSink{}, SessionPersona{}, nil)
	e.SetOutputForwarder(NewRealtimeOutputForwarder(c, "s1", "s1-ga", NewCitationResolver(GroundingContext{})))
	t.Cleanup(e.Close)
	e.Start()

	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventTranscriptDone, Text: "Here is the answer."}

	require.Eventually(t, func() bool {
		for _, env := range fs.sentEnvelopes() {
			if env.GetVoiceAgentRealtimeOutput() != nil &&
				env.GetVoiceAgentRealtimeOutput().GetText() == "Here is the answer." {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond, "transcript done must forward a VoiceAgentRealtimeOutput")
}

// TestRealtimeExecutor_ToolBridge_DispatchesAndContinues verifies the #458 MCP
// tool-bridge seam: an EventFunctionArgsDone runs the tool through the bridge,
// returns the result via SendFunctionResult, and chains a CreateResponse so the
// model continues with the result.
func TestRealtimeExecutor_ToolBridge_DispatchesAndContinues(t *testing.T) {
	fs := newFakeStream()
	c := newTestClient(t, fs)
	rt := newFakeRealtimeSession()
	e := NewRealtimeExecutor(context.Background(), CascadeConfig{SpaceID: "s1", GaAgentID: "s1-ga"},
		c, rt, &recordingSink{}, SessionPersona{}, nil)

	transport := func(_ context.Context, _ string, _ map[string]any) (string, bool, error) {
		return "tool output text", false, nil
	}
	bridge := NewMcpToolBridge(transport, nil,
		[]*memqlv1.ToolDefinition{td("webSearch", false, "read")}, "s1", "s1-ga", nil)
	bridge.RealtimeTools() // populate exposed set
	e.SetToolBridge(bridge)
	t.Cleanup(e.Close)
	e.Start()

	rt.events <- openai.RealtimeServerEvent{
		Kind: openai.EventFunctionArgsDone, CallID: "call_1", FuncName: "webSearch", Arguments: `{"q":"x"}`,
	}

	require.Eventually(t, func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return len(rt.functionResults) == 1 &&
			rt.functionResults[0].callID == "call_1" &&
			rt.functionResults[0].output == "tool output text"
	}, 2*time.Second, 10*time.Millisecond, "tool result returned via SendFunctionResult")

	require.Eventually(t, func() bool {
		return rt.responseCount() >= 1
	}, 2*time.Second, 10*time.Millisecond, "a CreateResponse continues the model with the tool result")
}

// findFinalTranscript returns the first VoiceAgentFinalTranscript among the
// recorded sends, or nil.
func findFinalTranscript(fs *fakeStream) *memqlv1.VoiceAgentFinalTranscript {
	for _, env := range fs.sentEnvelopes() {
		if f := env.GetVoiceAgentFinalTranscript(); f != nil {
			return f
		}
	}
	return nil
}

func hasTurnRequest(fs *fakeStream) bool {
	for _, env := range fs.sentEnvelopes() {
		if env.GetVoiceAgentTurnRequest() != nil {
			return true
		}
	}
	return false
}

// TestRealtimeExecutor_NativeMode_NoConductorRoundTrip verifies the #478 native
// gate: with native mode on, a (defensive) Deepgram final does NOT round-trip
// the conductor (no VoiceAgentTurnRequest) and drives no response.create -- the
// model owns the turn.
func TestRealtimeExecutor_NativeMode_NoConductorRoundTrip(t *testing.T) {
	fs := newFakeStream()
	replyToTurn(fs, "should not be requested")
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorForTest(t, fs, rt)
	e.SetTurnMode(turnModeNative)

	e.handleASRResult("user-1", polyphon.ASRResult{Text: "what's the plan", IsFinal: true})

	time.Sleep(80 * time.Millisecond)
	assert.False(t, hasTurnRequest(fs), "native mode must not round-trip the conductor")
	assert.Equal(t, 0, rt.responseCount(), "native mode: the model self-triggers, not the executor")
}

// TestRealtimeExecutor_NativeMode_InputTranscriptForwarded verifies the native
// human transcript path: the model's input-transcription events are forwarded
// as a partial + a native-authored final (so the server stamps it
// transcript-only), with no conductor round-trip.
func TestRealtimeExecutor_NativeMode_InputTranscriptForwarded(t *testing.T) {
	fs := newFakeStream()
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorForTest(t, fs, rt)
	e.SetTurnMode(turnModeNative)

	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventInputTranscriptDelta, Text: "cut the "}
	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventInputTranscriptDone, Text: "cut the cloud spend"}

	var final *memqlv1.VoiceAgentFinalTranscript
	require.Eventually(t, func() bool {
		final = findFinalTranscript(fs)
		return final != nil
	}, 2*time.Second, 10*time.Millisecond, "native input transcript must forward a final")

	assert.Equal(t, "cut the cloud spend", final.GetFinalText())
	assert.True(t, final.GetNativeAuthored(), "native human final must be marked native-authored")
	assert.False(t, hasTurnRequest(fs), "no conductor round-trip on the native path")
}

// newRealtimeExecutorNoStart builds a realtime executor WITHOUT Start(), so
// nothing drains transcriptCh -- letting a test saturate the forward queue to
// exercise enqueueTranscript's drop/guarantee semantics directly.
func newRealtimeExecutorNoStart(t *testing.T, fs *fakeStream, rt *fakeRealtimeSession) *RealtimeExecutor {
	t.Helper()
	c := newTestClient(t, fs)
	sink := &recordingSink{}
	e := NewRealtimeExecutor(context.Background(), CascadeConfig{
		SpaceID:   "s1",
		GaAgentID: "s1-ga",
		Thread:    memqlv1.VoiceAgentTurnRequest_THREAD_CONTEXT_TEAM,
	}, c, rt, sink, SessionPersona{Instructions: "be helpful", Voice: "marin"}, nil)
	t.Cleanup(e.Close)
	return e
}

// TestRealtimeExecutor_EnqueueFinal_NotDroppedUnderBackpressure verifies #1200:
// a user FINAL is never silently dropped behind a backlog of partials. The burst
// of input-transcription partials can saturate the forward queue right before the
// final lands; a naive drop-on-full there loses the user's whole utterance ("DB
// shows only assistant utterances"). A partial may still drop (cosmetic), but the
// final must reach the queue once a slot frees.
func TestRealtimeExecutor_EnqueueFinal_NotDroppedUnderBackpressure(t *testing.T) {
	fs := newFakeStream()
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorNoStart(t, fs, rt) // no consumer drains transcriptCh

	// Saturate the queue with partials.
	for i := 0; i < transcriptQueueDepth; i++ {
		e.enqueueTranscript(transcriptJob{speaker: "user-1", text: "partial"})
	}
	require.Len(t, e.transcriptCh, transcriptQueueDepth, "queue saturated with partials")

	// An extra partial drops on a full queue (cosmetic ghost-text only).
	e.enqueueTranscript(transcriptJob{speaker: "user-1", text: "dropped partial"})
	require.Len(t, e.transcriptCh, transcriptQueueDepth, "an extra partial drops on a full queue")

	// A final on a full queue must NOT drop -- it parks until a slot frees.
	e.enqueueTranscript(transcriptJob{speaker: "user-1", text: "cut the cloud spend", final: true, native: true})

	// Free one slot; the parked final must then land.
	<-e.transcriptCh
	var final transcriptJob
	require.Eventually(t, func() bool {
		for {
			select {
			case j := <-e.transcriptCh:
				if j.final {
					final = j
					return true
				}
			default:
				return false
			}
		}
	}, 2*time.Second, 10*time.Millisecond, "the user final must reach the queue, never dropped")
	assert.Equal(t, "cut the cloud spend", final.text)
	assert.True(t, final.native, "native-authored marking is preserved")
}

// TestRealtimeExecutor_InputTranscript_FiltersHallucination verifies #1199: a
// stock silence-hallucination phrase the realtime model fabricates from
// non-speech audio is DROPPED (not forwarded as a user utterance), while a real
// utterance still forwards. The realtime path carries no confidence signal, so
// the empty-drop + no-speech denylist do the work.
func TestRealtimeExecutor_InputTranscript_FiltersHallucination(t *testing.T) {
	fs := newFakeStream()
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorForTest(t, fs, rt)
	e.SetTurnMode(turnModeNative)

	// A canonical silence-hallucination phrase -> filtered.
	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventInputTranscriptDone, Text: "thank you for watching"}
	// An empty transcript -> filtered too.
	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventInputTranscriptDone, Text: "   "}
	time.Sleep(150 * time.Millisecond)
	assert.Nil(t, findFinalTranscript(fs), "hallucinated/empty input must not forward as a user utterance")

	// A genuine utterance still forwards normally.
	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventInputTranscriptDone, Text: "list the ten most beautiful birds"}
	var final *memqlv1.VoiceAgentFinalTranscript
	require.Eventually(t, func() bool {
		final = findFinalTranscript(fs)
		return final != nil
	}, 2*time.Second, 10*time.Millisecond, "real input must still forward")
	assert.Equal(t, "list the ten most beautiful birds", final.GetFinalText())
}

// replyToTurnWithDirective makes the fake server reply to a VoiceAgentTurnRequest
// with a conductor GATE directive (mode + brevity) instead of authored text --
// the #479 "WHEN not WHAT" path.
func replyToTurnWithDirective(fs *fakeStream, mode, brevity string) {
	fs.onSend = func(env *memqlv1.MemqlClientMessage) {
		if env.GetVoiceAgentTurnRequest() == nil {
			return
		}
		fs.push(&memqlv1.MemqlServerMessage{
			CorrelateTo: env.GetMessageId(),
			Payload: &memqlv1.MemqlServerMessage_VoiceAgentTurnComplete{
				VoiceAgentTurnComplete: &memqlv1.VoiceAgentTurnComplete{
					RequestId: "r1", UtteranceId: "u1",
					DirectiveMode: mode, Brevity: brevity,
				},
			},
		})
	}
}

// TestRealtimeExecutor_GateDirective_ModelAuthors verifies the #479 gate path:
// a directive (engage, primary, short) drives exactly one response.create whose
// instructions tell the MODEL to author its own reply (mode+brevity), never the
// "convey the following" authored-text framing.
func TestRealtimeExecutor_GateDirective_ModelAuthors(t *testing.T) {
	fs := newFakeStream()
	replyToTurnWithDirective(fs, "primary", "short")
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorForTest(t, fs, rt)

	e.handleASRResult("user-1", polyphon.ASRResult{Text: "how does oauth refresh work", IsFinal: true})

	require.Eventually(t, func() bool {
		return rt.responseCount() == 1
	}, 2*time.Second, 10*time.Millisecond, "engage directive drives one response.create")

	resp := rt.lastResponse()
	assert.Contains(t, resp, "Generate your own reply", "model authors on the gate path")
	assert.Contains(t, resp, "one short sentence", "brevity=short framing present")
	assert.NotContains(t, resp, "Convey the following", "no authored-text re-voice framing on the gate path")
}

// TestRealtimeExecutor_GateDirective_DeferSuppresses verifies a "defer" directive
// emits NO response.create -- the floor stays with the humans.
func TestRealtimeExecutor_GateDirective_DeferSuppresses(t *testing.T) {
	fs := newFakeStream()
	replyToTurnWithDirective(fs, "defer", "short")
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorForTest(t, fs, rt)

	e.handleASRResult("user-1", polyphon.ASRResult{Text: "mm, anyway", IsFinal: true})

	require.Eventually(t, func() bool {
		for _, env := range fs.sentEnvelopes() {
			if env.GetVoiceAgentTurnRequest() != nil {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 0, rt.responseCount(), "defer directive must not call response.create")
}

// TestRealtimeExecutor_GatedSemanticVad_AttributionNoTurn verifies the #481
// multi-party mode: a Deepgram final injects the labeled transcript for
// attribution but does NOT drive a turn (semantic_vad owns turn detection).
func TestRealtimeExecutor_GatedSemanticVad_AttributionNoTurn(t *testing.T) {
	fs := newFakeStream()
	replyToTurnWithDirective(fs, "primary", "short")
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorForTest(t, fs, rt)
	e.SetParticipant("user-1", "Maria", "Finance")
	e.SetTurnMode(turnModeGatedSemanticVad)

	e.handleASRResult("user-1", polyphon.ASRResult{Text: "cut cloud spend", IsFinal: true})

	time.Sleep(60 * time.Millisecond)
	// Attribution: the labeled transcript was injected as context.
	items := rt.injectedItems()
	require.NotEmpty(t, items, "the labeled transcript must be injected for attribution")
	assert.Contains(t, items[len(items)-1].Text, "cut cloud spend")
	// But Deepgram did NOT drive a turn (the model's turn-end does).
	assert.False(t, hasTurnRequest(fs), "Deepgram finals must not drive the turn in semantic_vad mode")
	assert.Equal(t, 0, rt.responseCount())
}

// TestRealtimeExecutor_GatedSemanticVad_InputTranscriptDrivesGate verifies that
// in #481 mode the model's turn-end (input transcript done) drives the conductor
// gate: a non-native final is forwarded and runTurn fires, so cognition runs the
// gate and the model authors on engage.
func TestRealtimeExecutor_GatedSemanticVad_InputTranscriptDrivesGate(t *testing.T) {
	fs := newFakeStream()
	replyToTurnWithDirective(fs, "primary", "short")
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorForTest(t, fs, rt)
	e.SetTurnMode(turnModeGatedSemanticVad)

	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventInputTranscriptDone, Text: "what's our cloud spend"}

	// The turn-request and the forwarded final transcript are two separate
	// envelopes emitted by the executor's goroutine; poll until BOTH are
	// present before asserting on the final, so we don't race the final
	// arriving a tick after the turn-request (capture inside the poll, the
	// pattern the native-path test above already uses).
	var final *memqlv1.VoiceAgentFinalTranscript
	require.Eventually(t, func() bool {
		final = findFinalTranscript(fs)
		return hasTurnRequest(fs) && final != nil
	}, 2*time.Second, 10*time.Millisecond,
		"the model's turn-end must drive the conductor gate (runTurn) and forward the final transcript")
	require.NotNil(t, final)
	assert.Equal(t, "what's our cloud spend", final.GetFinalText())
	assert.False(t, final.GetNativeAuthored(), "the gate path forwards a normal (stt) final so cognition runs the gate")
	// On the directive the model authors (one response.create).
	require.Eventually(t, func() bool { return rt.responseCount() == 1 }, 2*time.Second, 10*time.Millisecond)
	assert.Contains(t, rt.lastResponse(), "Generate your own reply")
}

// TestRealtimeExecutor_SpokenEqualsShown pins epic #475 / #482's core promise:
// the assistant's SPOKEN audio transcript is captured VERBATIM as the chat
// utterance -- there is no re-rendering between what the model said and what the
// record shows. Drives the model's output-transcript events through the
// executor and asserts the forwarded utterance text equals the spoken transcript.
func TestRealtimeExecutor_SpokenEqualsShown(t *testing.T) {
	fs := newFakeStream()
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorForTest(t, fs, rt)

	sender := &fakeOutputSender{ack: &memqlv1.VoiceAgentRealtimeOutputAck{Success: true, UtteranceId: "utt-1"}}
	e.SetOutputForwarder(NewRealtimeOutputForwarder(sender, "s1", "s1-ga", NewCitationResolver(GroundingContext{})))

	// The model streams its spoken transcript, then signals done with the full
	// text. Whatever it SAID is exactly what must be SHOWN.
	spoken := "The cloud spend is the place to start; it's the biggest line item."
	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventTranscriptDelta, Text: "The cloud spend is the place to start; "}
	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventTranscriptDelta, Text: "it's the biggest line item."}
	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventTranscriptDone, Text: spoken}

	require.Eventually(t, func() bool { return sender.lastSent() != nil }, 2*time.Second, 10*time.Millisecond,
		"the spoken transcript must be captured as an utterance")
	assert.Equal(t, spoken, sender.lastSent().GetText(),
		"shown utterance text must equal the spoken audio transcript verbatim")
}

// TestRealtimeExecutor_SpokenEqualsShown_AccumulatedDeltas verifies the same
// guarantee when the done event carries no text: the accumulated per-delta
// transcript IS the shown utterance (no token is dropped or rephrased).
func TestRealtimeExecutor_SpokenEqualsShown_AccumulatedDeltas(t *testing.T) {
	fs := newFakeStream()
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorForTest(t, fs, rt)
	sender := &fakeOutputSender{ack: &memqlv1.VoiceAgentRealtimeOutputAck{Success: true, UtteranceId: "utt-2"}}
	e.SetOutputForwarder(NewRealtimeOutputForwarder(sender, "s1", "s1-ga", NewCitationResolver(GroundingContext{})))

	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventTranscriptDelta, Text: "Yes, "}
	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventTranscriptDelta, Text: "I can help with that."}
	rt.events <- openai.RealtimeServerEvent{Kind: openai.EventTranscriptDone} // no text -> use accumulated

	require.Eventually(t, func() bool { return sender.lastSent() != nil }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, "Yes, I can help with that.", sender.lastSent().GetText())
}

// TestRealtimeExecutor_GateDirective_InjectsGrounding verifies the #490 grounding
// path: a directive carrying a grounding block injects it as a system item
// BEFORE the model generates, so the model-authored reply is grounded.
func TestRealtimeExecutor_GateDirective_InjectsGrounding(t *testing.T) {
	fs := newFakeStream()
	grounding := "Relevant context:\n[1] (finance) ARR is up 20% YoY."
	fs.onSend = func(env *memqlv1.MemqlClientMessage) {
		if env.GetVoiceAgentTurnRequest() == nil {
			return
		}
		fs.push(&memqlv1.MemqlServerMessage{
			CorrelateTo: env.GetMessageId(),
			Payload: &memqlv1.MemqlServerMessage_VoiceAgentTurnComplete{
				VoiceAgentTurnComplete: &memqlv1.VoiceAgentTurnComplete{
					RequestId: "r1", UtteranceId: "u1",
					DirectiveMode: "primary", Brevity: "short", Grounding: grounding,
				},
			},
		})
	}
	rt := newFakeRealtimeSession()
	e := newRealtimeExecutorForTest(t, fs, rt)

	e.handleASRResult("user-1", polyphon.ASRResult{Text: "how's revenue", IsFinal: true})

	require.Eventually(t, func() bool { return rt.responseCount() == 1 }, 2*time.Second, 10*time.Millisecond)
	// The grounding was injected as a system item.
	var found bool
	for _, it := range rt.injectedItems() {
		if it.Role == "system" && it.Text == grounding {
			found = true
		}
	}
	assert.True(t, found, "grounding must be injected as a system conversation item before generation")
	// And the model still authors (directive instructions, not the grounding text).
	assert.Contains(t, rt.lastResponse(), "Generate your own reply")
}
