package agents

// ai.go -- the `ai` builtin's executor: call a NAMED prompt from a MemQL body.
//
// ===========================================================================
// WHY THIS EXISTS
// ===========================================================================
// MemQL had an `ai()` expression and lost both of its front ends BY ACCIDENT.
// The projection form lived only in a hand-written parser a replacement never
// re-implemented, and the form that actually ran was a string match in shape
// steps, deleted with the legacy body grammar. Neither retirement was a
// decision: `ai(` appears nowhere in parser.V1RetiredForms, which is the list
// naming every deliberately retired spelling with its replacement.
//
// What survived is the whole of the work. MemQLEngine.InvokeAI resolves the
// prompt, validates the data against the prompt's own body (that body IS the
// input schema), renders the template, builds a core/airoute request carrying
// the prompt's @level, takes what the router returns, caches on the exact
// render hash, and journals the call so a work run can replay it. Three
// surfaces went on advertising `ai()` to authors and to models the whole time.
// This file is the missing seam, and nothing below it changed.
//
// ===========================================================================
// THE NAME AND THE SIGNATURE
// ===========================================================================
// The published vocabulary already said `ai(templateId string, data object,
// provider? string)`, so `ai` is the name and `templateId` / `data` are the
// argument names: a vocabulary that has been telling authors and models one
// spelling is not improved by shipping a different one.
//
// `provider` IS DROPPED, and the published entry is changed to match rather
// than the other way round. Every other disagreement between the two is worse
// than this one:
//
//   - A call site naming a model or a provider is a release every time the
//     fleet changes, which is the whole reason @level exists (epic memql#5127).
//     A level plus the rules decide; a body saying otherwise is a routing
//     decision written where no rule can see it and no decision record
//     explains it.
//   - A pin is not even usable for most of what it would name here.
//     TestNoPaidDefault refuses a prompt pinned to a federated provider, and
//     every concrete provider record this repository ships is federated -- so
//     an author reaching for the third argument would mostly be reaching for
//     something the build gate exists to refuse.
//   - The pin that survives is @defaultProvider ON THE PROMPT, where it is
//     read at load, checked against the declared providers, and refused when
//     it names a policy. That is a reviewable place for an override. An
//     argument is not.
//
// So the signature is `ai(templateId string, data object)`, both required.
// `data` is required rather than defaulted to `{}` because the prompt's body
// is a closed schema validated before the template renders: an omitted
// argument would be refused anyway, one layer down, as a missing FIELD -- and
// "your data object left out `run`" is a worse sentence than the call having
// to say what it is sending.
//
// ===========================================================================
// WHY THE EXECUTOR LIVES HERE
// ===========================================================================
// Beside runAgentTurn, which is the same shape: a synchronous, Go-backed AI
// call a body reaches through a builtin. IntegrationEngineAccess.InvokeAI is
// on this package's engine handle already, and its own doc comment has said
// for its whole life that it is `ai("<id>", {...data})`'s equivalent -- the
// seam was declared here and simply had no caller from the DSL side.
//
// The agents integration is a CORE plug-in, so this loads on every node type,
// and its handler touches i.agents not at all: calling a prompt is not an
// agents concern, and nothing here resolves an agent, an owner or a registry.
// What it shares with its neighbours is the engine handle and the refusal
// style, which is what makes this the right shelf rather than a new one.
//
// ===========================================================================
// THE GATE: NO @sdk, NO @requiresCapability, AND BOTH ARE DECISIONS
// ===========================================================================
// A builtin's annotation set is closed -- @alias, @args, @executor,
// @requiresCapability, @sdk, plus the lifecycle flags. There is no
// @serverOnly and no @requiresRank to reach for.
//
// NO @sdk is the wire decision, in the words dsl/platform/builtins.memql
// already uses for customDomainReconcile: "the absence of @sdk IS the wire
// decision". `ai` is a body primitive. A generated client method for it would
// be a button marked "spend money on a provider" with no row, no run and no
// owner in the path.
//
// NO @requiresCapability, and the reason is not the one it is tempting to
// write. A capability gate here would not be a spending control, because it
// would not run on the path that spends.
//
//   - INTERNAL ORIGIN PASSES THE CAPABILITY GATE UNCONDITIONALLY
//     (component/memql/requires_capability.go), and a SHIPPED automation's
//     body runs under internal origin: automations/executor.go's
//     originForSource stamps internal when the source came from the registered
//     tree. So a capability on this builtin would be invisible to every
//     automation in dsl/ -- which is the caller that actually makes the calls.
//   - What it WOULD reach is the untrusted branch of that same rule:
//     caller-submitted source -- an authored construct, an inline automation,
//     a bundle dry-run -- which originForSource deliberately holds at CLIENT
//     origin. That branch already has a trust gate, and it is about AUTHORING
//     rather than about money. Adding `execute construct` (owner and developer
//     only, seeded as "run inline DSL") would mean no authored automation may
//     call a prompt, which is a product decision nobody asked for, dressed as
//     a budget control.
//   - It matches every neighbour in dsl/agents/builtins.memql, which is the
//     answer to the obvious objection: runAgentTurn drives a whole tool loop
//     and carries no capability annotation either.
//
// WHAT ACTUALLY BOUNDS THE SPEND, stated plainly because the honest answer is
// smaller than it looks: `ai_guard.go` and nothing else. Its identical-request
// breaker and its process-wide rate ceiling sit on the RoundTripper every
// provider client is built from, so they catch a runaway before a vendor
// request regardless of who called.
//
// IN PARTICULAR, A RUN'S CEILINGS DO NOT APPLY. The call IS journalled -- the
// seam in ai_runtime.go records it on the run -- but nothing reads the journal
// to enforce a ceiling: component/work.CheckCeilings has NO production caller,
// only tests. An operator who sets a goal's costCeiling expecting it to bound
// prompt calls is bounding nothing, and a comment here promising otherwise
// would be worse than silence.
//
// ===========================================================================
// WHAT IT ANSWERS WITH, AND WHAT IT REFUSES
// ===========================================================================
// One MemoryNode carrying {prompt, reply}. `reply` is the key runAgentTurn
// already uses for the model's words, so a body reading what a model said
// reads one key name across both AI builtins rather than two.
//
// Every failure is a REFUSAL naming its cause, never an empty reply -- the
// reasoning runAgentTurn spells out for "answers only on an agent node": an
// empty answer is indistinguishable from a model that had nothing to say, and
// an unconfigured runtime, an unknown prompt and a data object that does not
// fit the schema are all deployment or authoring facts rather than answers.
// The three sentences a caller sees come from three different layers and each
// names what is actually wrong:
//
//	no engine handle       "the AI runtime is unreachable from this node"
//	no aiRuntime           "AI runtime is not configured"          (InvokeAI)
//	unknown prompt         `unknown prompt template "x"`           (aiRuntime)
//	data fails the schema  `data for prompt "x" invalid: ...`      (aiRuntime)
//	no router wired        ErrAIResolverUnwired                    (aiRuntime)
//
// This file adds the first and wraps the rest with the call's own name; it
// swallows none of them and substitutes for none of them.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// invokePromptCapName is the capability the `ai` builtin's @executor names.
// Spelled out rather than "ai" because a Go capability called `ai` says
// nothing at the call site; the builtin keeps the short name the vocabulary
// publishes, and the two are joined by the @executor string, as `agent` and
// `invoke` already are.
const invokePromptCapName = "invokePrompt"

// handleInvokePrompt is the `ai` builtin's executor.
//
//	args["templateId"]  string  required -- the prompt to call, by name
//	args["data"]        object  required -- the prompt's declared input fields
//
// SYNCHRONOUS, for runAgentTurn's reason: a body statement is already running
// inside whatever detached the work, and a second layer of asynchrony here
// would be a run waiting on something it could not journal.
func (i *Integration) handleInvokePrompt(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil {
		return nil, fmt.Errorf("ai: agents integration not initialized")
	}
	templateId := strings.TrimSpace(asString(args["templateId"]))
	if templateId == "" {
		return nil, fmt.Errorf("ai: 'templateId' is required -- name the prompt to call")
	}
	data, err := promptData(templateId, args["data"])
	if err != nil {
		return nil, err
	}
	if i.engine == nil {
		// NAMED, not empty, and not a silent `{}` answer. This is the node
		// saying it cannot reach the AI runtime at all, which is a deployment
		// fact; the layers below name their own causes.
		return nil, fmt.Errorf("ai(%q): no engine handle on this node, so the AI runtime is unreachable and the prompt cannot be called", templateId)
	}

	// THE LIVE SEAM. Everything a prompt call is -- resolve the prompt,
	// validate the data against its body, render, route by @level through
	// core/airoute, cache, journal -- happens inside this one call, and none
	// of it is re-implemented here.
	reply, err := i.engine.InvokeAI(ctx, templateId, data)
	if err != nil {
		return nil, fmt.Errorf("ai(%q): %w", templateId, err)
	}

	payload, err := json.Marshal(map[string]any{
		"prompt": templateId,
		"reply":  reply,
	})
	if err != nil {
		return nil, fmt.Errorf("ai(%q): marshal envelope: %w", templateId, err)
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("ai-envelope:%s:%d", templateId, time.Now().UnixNano()),
		Concept:   envelopeConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		CreatedBy: systemActorId,
		Payload:   payload,
	}}, nil
}

// promptData coerces the `data` argument to the map InvokeAI takes.
//
// A MISSING data object is refused rather than sent as {}. The prompt's body
// is a closed schema, so an empty map reaches the validator and comes back as
// a complaint about a missing FIELD -- which reads as "the prompt is wrong"
// when what happened is that the call forgot its second argument.
//
// A PRESENT but wrongly-typed one is refused by the same rule and named by its
// Go type, because `data: args.thing` where `thing` is a string is the natural
// mistake and the validator's message for it would be about the schema.
func promptData(templateId string, raw any) (map[string]any, error) {
	switch v := raw.(type) {
	case nil:
		return nil, fmt.Errorf("ai(%q): 'data' is required -- pass the object whose fields the prompt declares (an empty object is `{}`)", templateId)
	case map[string]any:
		return v, nil
	default:
		return nil, fmt.Errorf("ai(%q): 'data' must be an object naming the prompt's declared fields, not %T", templateId, raw)
	}
}
