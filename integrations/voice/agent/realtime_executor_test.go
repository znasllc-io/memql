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
	e.SetNativeMode(true)

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
	e.SetNativeMode(true)

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
