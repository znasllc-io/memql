package memql

// caller_context_test.go -- A STREAM HANDLER THAT REACHES A MODEL CARRIES ITS
// CALLER (owner decision 2026-11-23).
//
// The verifier interceptor stamps claims onto a stream's context and nothing
// else; ensureAccess turns them into the AccessContext every engine gate and
// the model router read. A handler that read s.stream.Context() and called a
// model without it made a call that named nobody. While a call naming nobody
// counted as the cluster's own work, the cluster's vendor account paid for it
// with no permission asked; now such a call is refused that account. So each
// handler that reaches a model on the node it arrived at hands the model its
// caller: Ask's suggestions when no agent replica is reachable, a streaming
// transcription opened on the node that holds the speech provider, and an
// agent turn on an agent node.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/stt"
)

// callerSession is a direct (not forwarded) stream whose context carries no
// access context, the way the verifier interceptor leaves it, and whose
// session has already resolved its caller.
func callerSession(t *testing.T, svc *service) (*streamSession, *auth.AccessContext) {
	t.Helper()
	actor := &auth.AccessContext{UserId: "v1:identity:user:olga", Role: auth.RoleWriter}
	stream := newCaptureStream(t)
	return &streamSession{
		service: svc, stream: stream, logger: testLogger(),
		access: actor, accessLoaded: true, credentialClass: auth.ForwardedClassUser, badgeStamped: true,
	}, actor
}

// requireCaller fails unless ctx carries the session's own caller.
func requireCaller(t *testing.T, ctx context.Context, actor *auth.AccessContext, what string) {
	t.Helper()
	got, ok := auth.AccessFromContext(ctx)
	require.Truef(t, ok, "%s carried no caller: the model call named nobody", what)
	require.Equalf(t, actor.UserId, got.UserId, "%s carried the wrong caller", what)
}

// suggestCallerModel is the router and the structured model in one: it
// remembers the context of the resolution and of the call.
type suggestCallerModel struct {
	resolved chan context.Context
	called   chan context.Context
}

func (m *suggestCallerModel) ResolveFor(ctx context.Context, _ airoute.ResolveRequest) (memqlengine.ResolvedProvider, error) {
	m.resolved <- ctx
	return memqlengine.ResolvedProvider{Client: m}, nil
}

func (m *suggestCallerModel) CallChatStructured(ctx context.Context, _ []common.ChatMessage, _ common.StructuredSchema) (string, error) {
	m.called <- ctx
	return `{"title":"fixture"}`, nil
}

const suggestCallerDomain = "callerContextFixture"

var registerSuggestCallerDomain sync.Once

func TestAskSuggestionsAnsweredHereCarryTheCaller(t *testing.T) {
	registerSuggestCallerDomain.Do(func() {
		memqlengine.RegisterSuggestDomain(suggestCallerDomain, func(memqlengine.SuggestContext) (memqlengine.SuggestPlan, error) {
			return memqlengine.SuggestPlan{
				Messages:   []common.ChatMessage{{Role: "user", Content: "suggest"}},
				SchemaName: "fixtureSuggest",
				Schema:     []byte(`{"type":"object"}`),
			}, nil
		})
	})
	model := &suggestCallerModel{resolved: make(chan context.Context, 1), called: make(chan context.Context, 1)}
	engine := &memqlengine.MemQLEngine{}
	engine.SetAIResolver(model)
	session, actor := callerSession(t, &service{engine: engine, logger: testLogger()})

	payload, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)
	msg := &memqlv1.AiSuggestMsg{RequestId: "suggest-1", Domain: suggestCallerDomain, Payload: payload}
	envelope := &memqlv1.MemqlClientMessage{MessageId: "envelope-suggest-1", Payload: &memqlv1.MemqlClientMessage_AiSuggest{AiSuggest: msg}}
	require.NoError(t, session.handleAiSuggest(envelope, msg))

	for what, ch := range map[string]chan context.Context{"the resolution": model.resolved, "the model call": model.called} {
		select {
		case ctx := <-ch:
			requireCaller(t, ctx, actor, what)
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never happened", what)
		}
	}
}

// turnCallerHandler is an agent's replier that remembers the context a turn
// was handed.
type turnCallerHandler struct {
	got chan context.Context
}

func (h *turnCallerHandler) Handle(ctx context.Context, _ *memqlv1.AgentGenerateTurnMsg, _ AgentTurnSink) (*AgentTurnResult, error) {
	h.got <- ctx
	return &AgentTurnResult{}, nil
}

func TestAnAgentTurnStartedHereCarriesTheCaller(t *testing.T) {
	replier := &turnCallerHandler{got: make(chan context.Context, 1)}
	session, actor := callerSession(t, &service{logger: testLogger(), agentReplier: replier})

	msg := &memqlv1.AgentGenerateTurnMsg{RequestId: "turn-1"}
	envelope := &memqlv1.MemqlClientMessage{MessageId: "envelope-turn-1", Payload: &memqlv1.MemqlClientMessage_AgentGenerateTurn{AgentGenerateTurn: msg}}
	require.NoError(t, session.handleAgentGenerateTurn(envelope, msg))

	select {
	case ctx := <-replier.got:
		requireCaller(t, ctx, actor, "the agent turn")
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never reached the replier")
	}
}

// transcribeCallerProvider is a speech provider that remembers the context a
// session was opened with, and opens none.
type transcribeCallerProvider struct {
	got chan context.Context
}

func (p *transcribeCallerProvider) StartStream(ctx context.Context, _ stt.StreamConfig) (stt.StreamingSession, error) {
	p.got <- ctx
	return nil, errors.New("fixture: no session")
}

func (*transcribeCallerProvider) Name() string { return "fixture" }

func TestATranscriptionOpenedHereCarriesTheCaller(t *testing.T) {
	provider := &transcribeCallerProvider{got: make(chan context.Context, 1)}
	session, actor := callerSession(t, &service{logger: testLogger(), sttProvider: provider})

	msg := &memqlv1.AiTranscribeStreamStart{RequestId: "transcribe-1"}
	envelope := &memqlv1.MemqlClientMessage{MessageId: "envelope-transcribe-1", Payload: &memqlv1.MemqlClientMessage_AiTranscribeStreamStart{AiTranscribeStreamStart: msg}}
	_ = session.handleAiTranscribeStreamStart(envelope, msg)

	select {
	case ctx := <-provider.got:
		requireCaller(t, ctx, actor, "the transcription session")
	case <-time.After(5 * time.Second):
		t.Fatal("the transcription never reached the provider")
	}
}
