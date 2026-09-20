package memql

// app_session_provider.go -- the `session` door's client and the seam behind it
// (epic memql#5391, task memql#5392, design D7).
//
// THE INVERSION, TAKEN ONE STEP FURTHER. app_provider.go already explains why
// an app is the agent and MemQL its provider; it also explains why the app door
// deliberately does NOT serve MemQL's own tool-calling turns -- a tool turn is
// MemQL driving, and driving an app that is itself driving produces two agents
// fighting over one conversation.
//
// That was the right answer to the wrong question. On a cluster whose only open
// door is a signed-in Claude Code, "the app cannot serve this turn" meant the
// router passed the door over and Ask stayed dark. The right answer is that the
// app does not serve the TURN; it takes the whole STEP. The step goes over
// intact -- prompt, inputs, workspace, MCP endpoint, limits, response schema --
// the app drives its own loop, and what comes back is ONE assistant turn with
// NO tool calls, which is exactly what ends MemQL's loop without a second
// iteration.
//
// A SESSION IS OPENED FROM A STEP, which is why a call carrying none is refused
// rather than served. A bare Go model call with tools has no step to hand over,
// and that is a fact about the call site -- reporting it as an unavailable door
// would send somebody to look at their laptop.
//
// THE DELEGATE IS A SEAM, for the reason AppInference is one: a session travels
// over the WorkerService stream, which terminates on the agent node, so the
// code that can open one is behind `//go:build agent` while this package is
// linked into every binary. An UNWIRED delegate REFUSES and does not fall back
// -- ai_resolver.go's rule, applied to the second seam. A seam whose setter is
// never called is green, silent and inert, and the tempting nil-handling here
// ("just serve it as a chat turn") is precisely the two-agents failure the
// door exists to avoid.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/core/common"
)

// ErrAppSessionNoStep is the refusal for a tool-needing call that carries no
// work step. It is a distinct error rather than a refusal code because the fix
// is at the CALL SITE -- give the call a step, or do not route it through an
// app -- and not in a policy, a machine or a vendor account.
var ErrAppSessionNoStep = errors.New(
	"an app door serves a tool-needing call by taking the whole step as a session, and a session is " +
		"opened from a step: this call carries none, so there is nothing to hand over")

// AppSessionHandover is one delegated step, as the door hands it over.
type AppSessionHandover struct {
	// ActingUserId scopes the session to that user's machines and is the
	// subject of the per-run back-channel credential. EMPTY IS A REFUSAL
	// downstream, never a wildcard.
	ActingUserId string
	AgentId      string
	// AppId is the door that was resolved: one of the engine's closed set.
	AppId string
	// Model is the model a policy PINNED with `app:<id>:<model>`. EMPTY IS NO
	// PIN, and the app runs at whatever its own default is.
	Model string
	// Level is the call's level, one of core/airoute's closed four. The
	// COCKPIT owns the translation into the app's own knobs.
	Level string
	// RunId and StepId name the step being handed over. StepId is required:
	// see ErrAppSessionNoStep.
	RunId  string
	StepId string
	// Prompt is the conversation, flattened. An app takes a prompt, not a
	// message array: it is going to hold its own conversation from here.
	Prompt string
	// Tools are the tools the turn offered, carried so the delegate can tell
	// the app what MemQL can do. They never become a tool loop on this side --
	// the app reaches these over MCP, in the other direction.
	Tools []common.ToolDefinition
	// ResponseSchema is the output contract the call declared, when it
	// declared one. Nil means none was asked for, which is not the same state
	// as asking and getting nothing back.
	ResponseSchema *common.StructuredSchema
	// Inputs are Library artifact ids the cockpit pulls into the session
	// workspace before the run starts.
	Inputs []string
}

// AppSessionOutcome is what the session produced.
type AppSessionOutcome struct {
	// Content is the text answer -- the session's transcript or its structured
	// answer rendered, whichever the delegate judged the reply to be.
	Content string
	// Result is the harness's STRUCTURED final answer as raw JSON, when it
	// produced one. It does NOT imply success: a harness can answer the schema
	// and still exit non-zero, and folding the two would make an answer we
	// have unreadable because the run that produced it also failed.
	Result []byte
	// ChildRunId is the subrun the step now points at (design D7).
	ChildRunId string
	// SessionId is the v1:worker:appSession row.
	SessionId string
	// ArtifactIds are the Library artifacts the session produced.
	ArtifactIds []string
	// Model and Effort are what the APP REPORTED, never what was asked for
	// (design D9). Empty means it did not say, which is recorded as unknown.
	Model  string
	Effort string
	// Billing is "subscription" when the app reported one and "unknown" when
	// it said nothing. Never "metered": MemQL was not billed for a call it did
	// not make.
	//
	// THE TOKEN COUNTS ARE DELIBERATELY NOT HERE. The session's own reported
	// spend is already written to v1:router:call by the executor's ledger
	// writer, from the runner's RunResult -- which carries the app's `known`
	// flag. A second copy taken off the executor's output map would have the
	// count and NOT the flag, so "the app reported zero" and "the app said
	// nothing" would arrive here as the same value. Two places for one fact
	// are two places for it to disagree, and this is the one where the
	// disagreement would be silent.
	Billing string
	// ExecutionSurface names where the session ran, for the decision row.
	ExecutionSurface string
}

// AppSessionDelegate is the seam an agent-tagged build fills in. It mirrors
// AppInference for the reason stated in app_provider.go: a session travels over
// the WorkerService stream, which terminates on the agent node.
type AppSessionDelegate interface {
	// RunStep opens a session for the handed-over step and runs it to
	// completion. An error is a refusal the caller surfaces; there is no
	// second-choice path.
	RunStep(ctx context.Context, h AppSessionHandover) (AppSessionOutcome, error)
}

// SetAppSessionDelegate installs the implementation. Called once during cluster
// wiring on a build that has a worker service; every other build leaves it nil
// and refuses a session as a wiring fault.
func (r *ProviderRegistry) SetAppSessionDelegate(d AppSessionDelegate) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appSessions = d
}

// AppSessionDelegateInstalled reports whether this node can hand a step to an
// app at all. It exists for the boot check and for the wiring test; nothing on
// the call path branches on it, because a branch would be the fallback this
// file refuses.
func (r *ProviderRegistry) AppSessionDelegateInstalled() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.appSessions != nil
}

// SessionRequest is the call's own identity, carried into every handover.
type SessionRequest struct {
	ActingUserId string
	AgentId      string
	Level        string
	RunId        string
	StepId       string
}

// SessionProvider builds the client behind a `session` winner.
//
// It returns `any` for the reason ResolvedProvider.Client is `any`: the
// modalities do not share an interface, and the router hands this straight back
// to a call site that type-asserts to the one it asked for. The value satisfies
// both tool-calling surfaces, plus the two structural reporters the decision
// row reads.
func (r *ProviderRegistry) SessionProvider(appId, model string, req SessionRequest) any {
	return &sessionProvider{registry: r, appId: appId, model: model, req: req}
}

// sessionProvider hands a whole step to an app session.
type sessionProvider struct {
	registry *ProviderRegistry
	appId    string
	model    string
	req      SessionRequest

	lastMu     sync.Mutex
	lastResult AppSessionOutcome
	lastSet    bool
}

func (p *sessionProvider) delegate() AppSessionDelegate {
	if p == nil || p.registry == nil {
		return nil
	}
	p.registry.mu.RLock()
	defer p.registry.mu.RUnlock()
	return p.registry.appSessions
}

// run is the one path both surfaces take.
func (p *sessionProvider) run(
	ctx context.Context,
	messages []common.ChatMessage,
	tools []common.ToolDefinition,
	schema *common.StructuredSchema,
) (AppSessionOutcome, error) {
	if strings.TrimSpace(p.req.StepId) == "" {
		return AppSessionOutcome{}, fmt.Errorf("app door %q: %w", p.appId, ErrAppSessionNoStep)
	}
	d := p.delegate()
	if d == nil {
		// A WIRING FAULT, said as one. "No app-session delegate is installed"
		// and "no machine can run this app" need different fixes and the
		// sentences must not be confusable -- the second sends somebody to
		// open a laptop for a problem in app/.
		return AppSessionOutcome{}, fmt.Errorf(
			"app door %q resolved and this node has no app-session delegate installed: handing a step to "+
				"an app needs a worker service, and this is a boot-wiring fault rather than a shut door", p.appId)
	}

	actingUser := strings.TrimSpace(p.req.ActingUserId)
	if actingUser == "" {
		actingUser = actingUserFromContext(ctx)
	}

	out, err := d.RunStep(ctx, AppSessionHandover{
		ActingUserId:   actingUser,
		AgentId:        p.req.AgentId,
		AppId:          p.appId,
		Model:          p.model,
		Level:          p.req.Level,
		RunId:          p.req.RunId,
		StepId:         p.req.StepId,
		Prompt:         flattenForSession(messages),
		Tools:          tools,
		ResponseSchema: schema,
	})
	if err != nil {
		return out, err
	}
	p.lastMu.Lock()
	p.lastResult = out
	p.lastSet = true
	p.lastMu.Unlock()
	return out, nil
}

// CallChatWithTools implements common.ToolCallingChatAIProvider.
//
// THE ANSWER CARRIES NO TOOL CALLS, and that is the whole shape of this door.
// The app already ran its own loop and called whatever it needed over MCP; a
// tool call here would be MemQL asking an agent that has finished to carry on,
// which is the two-agents failure by another route. An empty ToolCalls slice is
// what ends the caller's loop on the first iteration.
func (p *sessionProvider) CallChatWithTools(
	ctx context.Context,
	messages []common.ChatMessage,
	tools []common.ToolDefinition,
) (*common.ToolCallingChatResult, error) {
	out, err := p.run(ctx, messages, tools, nil)
	if err != nil {
		return nil, err
	}
	return &common.ToolCallingChatResult{AssistantText: out.Content}, nil
}

// CallChatStreamWithTools implements common.ChatStreamWithToolsProvider.
//
// A SESSION DOES NOT STREAM ITS ANSWER HERE, and the single chunk is honest
// rather than lazy: the session's own live output is the transcript, which the
// cockpit streams onto the v1:worker:appSession row and the Work app reads
// there. Re-emitting it as model deltas would make a record of ACTIONS look
// like a model thinking, and the caller would then be free to interleave it
// with a tool loop that is not running.
func (p *sessionProvider) CallChatStreamWithTools(
	ctx context.Context,
	messages []common.ChatMessage,
	tools []common.ToolDefinition,
) (<-chan common.StreamToolChunk, error) {
	out, err := p.run(ctx, messages, tools, nil)
	if err != nil {
		return nil, err
	}
	ch := make(chan common.StreamToolChunk, 2)
	if out.Content != "" {
		ch <- common.StreamToolChunk{Content: out.Content}
	}
	ch <- common.StreamToolChunk{Done: true}
	close(ch)
	return ch, nil
}

// ExecutionSurface implements the router's structural surface reporter: where
// the call ran, for the decision row.
func (p *sessionProvider) ExecutionSurface() string {
	if p == nil {
		return ""
	}
	p.lastMu.Lock()
	defer p.lastMu.Unlock()
	return p.lastResult.ExecutionSurface
}

// ServedModel implements the router's structural served-model reporter: what
// the APP REPORTED serving this call with, and at what effort (design D9).
//
// Two empty strings before the first call, and two empty strings when the app
// said nothing. Neither is filled in from p.model -- a servedModel copied from
// the request would record as measured something nobody measured, and the gap
// between what was pinned and what ran is the only way to see an app that
// rerouted or ignored the pin.
func (p *sessionProvider) ServedModel() (string, string) {
	if p == nil {
		return "", ""
	}
	p.lastMu.Lock()
	defer p.lastMu.Unlock()
	return p.lastResult.Model, p.lastResult.Effort
}

// Billing reports who PAID for the most recent session, for the router's
// decision row. A session runs inside somebody's own subscription, so it is
// "subscription" when the app reported one and "unknown" when it said nothing
// -- never "metered", because MemQL was not billed for work it did not buy.
//
// Without this the router's row would say `metered` for a whole step run on a
// person's own quota, and a cost reader separating "what we spent" from "what
// ran somewhere we do not pay" would have had every session on the wrong side.
func (p *sessionProvider) Billing() string {
	if p == nil {
		return ""
	}
	p.lastMu.Lock()
	defer p.lastMu.Unlock()
	return p.lastResult.Billing
}

// LastSession reports the session the most recent call ran as, for a caller
// that needs the subrun id. Ok is false before the first call.
func (p *sessionProvider) LastSession() (AppSessionOutcome, bool) {
	if p == nil {
		return AppSessionOutcome{}, false
	}
	p.lastMu.Lock()
	defer p.lastMu.Unlock()
	return p.lastResult, p.lastSet
}

var (
	_ common.ToolCallingChatAIProvider   = (*sessionProvider)(nil)
	_ common.ChatStreamWithToolsProvider = (*sessionProvider)(nil)
)

// flattenForSession renders a conversation as the single prompt an app takes.
//
// It is a SECOND copy of integrations/agent/worker's flattenMessages, and
// deliberately so: that one lives behind `//go:build agent` in a package this
// one cannot import -- the dependency points the other way. The two are small,
// and a shared home for them would be a third package existing only to hold
// nine lines.
func flattenForSession(messages []common.ChatMessage) string {
	var b strings.Builder
	for _, m := range messages {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		switch strings.ToLower(m.Role) {
		case "system":
			b.WriteString("[system]\n")
		case "assistant":
			b.WriteString("[assistant]\n")
		case "user", "":
			// The unlabelled default: a single-turn prompt is by far the
			// common case and labelling it adds noise the app reads past.
		default:
			b.WriteString("[" + m.Role + "]\n")
		}
		b.WriteString(strings.TrimSpace(m.Content))
	}
	return b.String()
}
