package router

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// Engine is the narrow engine surface the router needs to write
// v1:router:call rows. Signature matches *memql.MemQLEngine.Execute so
// the concrete engine can be passed directly; no adapter required.
type Engine interface {
	Execute(ctx context.Context, query string) (*memql.ExecuteResult, error)
}

// Router is the MemQL AI Router. It resolves a ResolveRequest to a
// provider (or a policy-driven fallback chain), wraps that provider
// with observability, and writes one v1:router:call row per call.
// Concurrent-safe; a single instance is shared across every node that
// makes AI calls.
type Router struct {
	providers *memql.ProviderRegistry
	policies  *memql.PolicyRegistry
	rules     *memql.RuleRegistry
	engine    Engine
	logger    *slog.Logger

	// recordsDropped tracks how many v1:router:call writes were
	// skipped because the engine was unavailable, the queue was full,
	// or the write returned an error. Exposed for health metrics; not
	// on the hot path.
	recordsDropped atomic.Uint64

	// ledger is the bounded queue of pending decision records and
	// ledgerOnce starts its single writer (memql#5581). See
	// LedgerQueueDepth for why the ledger is one goroutine and one
	// bounded queue rather than a goroutine per record.
	ledger     chan CallRecord
	ledgerOnce sync.Once

	// ceilingCheck is the cost-ceiling probe the federation hop consults
	// (epic memql#5096, design D4). It defaults to memql.CostCeilingReached
	// and is a FIELD so a test can drive the refusal without mutating the
	// process-wide guard every other test in the tree shares -- the same
	// reason ai_guard_fleet.go keeps admitLocalCall separate from its
	// package-level wrapper.
	ceilingCheck func(context.Context) (string, bool)
}

// New constructs a Router. The provider, policy and RULE registries are all
// required; the engine is required for ledger writes; the logger may be nil.
//
// A NIL RULE REGISTRY IS A REFUSAL AT RESOLVE TIME, NOT A FALLBACK, and that
// is the one thing about this constructor worth stating. Without rules there
// is no `default` rule, so there is no chain to walk -- and the only
// alternative to refusing would be for the router to pick one itself, which is
// precisely the pre-rules precedence (explicit, then a caller-named policy,
// then a default provider) this epic removed. Re-appearing silently is how it
// would come back: every call would resolve, the ledger would look ordinary,
// and no decision record would name a rule.
//
// It is not refused HERE because a constructor that returns an error is a
// change at every call site including the tests, and because the honest place
// to refuse is where the missing thing is needed.
func New(providers *memql.ProviderRegistry, policies *memql.PolicyRegistry, rules *memql.RuleRegistry, engine Engine, logger *slog.Logger) *Router {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Router{
		providers:    providers,
		policies:     policies,
		rules:        rules,
		engine:       engine,
		logger:       logger,
		ceilingCheck: memql.CostCeilingReached,
	}
}

// ResolveStreamWithTools picks a provider chain for a streaming
// chat-with-tools call and returns a wrapped provider. On pre-flight
// error the wrapper automatically advances down the fallback chain,
// recording each failed attempt with outcome="fallback_used".
func (r *Router) ResolveStreamWithTools(req ResolveRequest) (common.ChatStreamWithToolsProvider, Resolved, error) {
	return r.resolveStreamWithTools(context.Background(), req)
}

func (r *Router) resolveStreamWithTools(ctx context.Context, req ResolveRequest) (common.ChatStreamWithToolsProvider, Resolved, error) {
	chain, resolved, err := r.resolveChain(ctx, req, modalityStreamTools)
	if err != nil {
		return nil, Resolved{}, err
	}
	req = r.stampRequestId(req)
	resolved.Chain = chain
	return &fallbackStreamWithTools{
		router:   r,
		chain:    chain,
		req:      req,
		resolved: resolved,
	}, resolved, nil
}

// ResolveWithTools picks a provider chain for a NON-streaming
// request/response chat-with-tools call -- the background / batch
// execution lane (memql#896). A planner-dispatched plan/task turn has no
// human watching tokens arrive, so it runs through the synchronous
// CallChatWithTools surface (one request, one response, one overall
// timeout) instead of the interactive streaming path + idle watchdog.
// Fallback chain semantics mirror ResolveStreamWithTools: a pre-flight
// error advances down the chain, recording each failed attempt as
// outcome="fallback_used".
func (r *Router) ResolveWithTools(req ResolveRequest) (common.ToolCallingChatAIProvider, Resolved, error) {
	return r.resolveWithTools(context.Background(), req)
}

func (r *Router) resolveWithTools(ctx context.Context, req ResolveRequest) (common.ToolCallingChatAIProvider, Resolved, error) {
	chain, resolved, err := r.resolveChain(ctx, req, modalityTools)
	if err != nil {
		return nil, Resolved{}, err
	}
	req = r.stampRequestId(req)
	resolved.Chain = chain
	return &fallbackWithTools{
		router:   r,
		chain:    chain,
		req:      req,
		resolved: resolved,
	}, resolved, nil
}

// ResolveChat picks a provider for a non-streaming synchronous chat call
// (suggest endpoints, voice-path InvokeAI turns) and returns the wrapped
// provider. Fallback chain semantics mirror ResolveStreamWithTools.
func (r *Router) ResolveChat(req ResolveRequest) (common.ChatAIProvider, Resolved, error) {
	return r.resolveChat(context.Background(), req)
}

func (r *Router) resolveChat(ctx context.Context, req ResolveRequest) (common.ChatAIProvider, Resolved, error) {
	chain, resolved, err := r.resolveChain(ctx, req, modalityChat)
	if err != nil {
		return nil, Resolved{}, err
	}
	req = r.stampRequestId(req)
	resolved.Chain = chain
	return &fallbackChat{
		router:   r,
		chain:    chain,
		req:      req,
		resolved: resolved,
	}, resolved, nil
}

// ResolveStructured picks a provider for a structured-output call -- the
// classifiers, the routing prompts, every CallChatStructured site.
//
// There is no fallback wrapper and no observer on this surface yet: the
// observed* wrappers cover the three chat surfaces, and wrapping a fourth
// without a ledger row to write would be an empty layer. The RESOLUTION is
// still recorded in full on Resolved.Decision.
func (r *Router) ResolveStructured(req ResolveRequest) (common.ChatStructuredProvider, Resolved, error) {
	client, resolved, err := r.resolveDirect(context.Background(), req, modalityStructured)
	if err != nil {
		return nil, Resolved{}, err
	}
	return client.(common.ChatStructuredProvider), resolved, nil
}

// ResolveVision picks a provider for a vision call -- an image or a document
// handed to a model that can look at it.
func (r *Router) ResolveVision(req ResolveRequest) (common.VisionAIProvider, Resolved, error) {
	client, resolved, err := r.resolveDirect(context.Background(), req, modalityVision)
	if err != nil {
		return nil, Resolved{}, err
	}
	return client.(common.VisionAIProvider), resolved, nil
}

// ResolveEmbedding picks a provider for an embedding call.
//
// The level for these is `embeddings`, which never degrades: a degraded
// embedder answers in a different vector space, so the result is not a worse
// vector but one that does not belong in the index it is about to be written
// to. The shipped `embeddingsPark` rule is what enforces that, not this
// function -- which is the point of levels being data.
func (r *Router) ResolveEmbedding(req ResolveRequest) (memql.EmbeddingAIProvider, Resolved, error) {
	client, resolved, err := r.resolveDirect(context.Background(), req, modalityEmbedding)
	if err != nil {
		return nil, Resolved{}, err
	}
	return client.(memql.EmbeddingAIProvider), resolved, nil
}

// resolveDirect resolves a modality that has no fallback wrapper: it runs the
// same chain walk and hands back the winning entry's client.
//
// The interface assertion is safe by construction -- walkChain refused every
// candidate that did not implement it -- but the entry is looked up once more
// here rather than threaded out of the walk, because the walk returns the
// chain the wrapper would use and this surface has no wrapper to give it to.
func (r *Router) resolveDirect(ctx context.Context, req ResolveRequest, mod providerModality) (any, Resolved, error) {
	chain, resolved, err := r.resolveChain(ctx, req, mod)
	if err != nil {
		return nil, Resolved{}, err
	}
	client, _, ok := r.providerLookup(ctx, req, resolved.ProviderName, mod)
	if ctx.Err() != nil {
		return nil, Resolved{}, ctx.Err()
	}
	if !ok {
		// The entry resolved a moment ago and does not now. A machine slept,
		// or a credential expired between the two lookups. It is a refusal
		// rather than a panic on a failed assertion, and it says which
		// provider moved.
		return nil, Resolved{}, fmt.Errorf("router: %s resolved for %s and was gone by the time it was used",
			resolved.ProviderName, modalityName(mod))
	}
	resolved.Chain = chain
	return client, resolved, nil
}

type providerModality int

const (
	modalityStreamTools providerModality = iota
	modalityChat
	modalityStreamChat
	// modalityTools is the non-streaming request/response tool-calling
	// surface (common.ToolCallingChatAIProvider) used by the background
	// execution lane (memql#896). Both the streaming providers
	// (anthropicStreamProvider / openAIStreamProvider) and the plain
	// non-streaming providers implement CallChatWithTools, so any chain
	// entry that serves modalityStreamTools also serves modalityTools.
	modalityTools
	// modalityStructured, modalityVision and modalityEmbedding are the three
	// non-chat surfaces the seam carries (design D2). Each maps to exactly one
	// provider interface.
	//

	modalityStructured
	modalityVision
	modalityEmbedding
	modalitySpeech
	modalityTranscribe
)

// modalityName is what the report calls a modality an entry does not serve.
func modalityName(mod providerModality) string {
	switch mod {
	case modalityStreamChat:
		return "streaming chat turns"
	case modalityStreamTools:
		return "streaming tool-calling turns"
	case modalityTools:
		return "tool-calling turns"
	case modalityStructured:
		return "structured output"
	case modalityVision:
		return "vision turns"
	case modalityEmbedding:
		return "embeddings"
	case modalitySpeech:
		return "speech"
	case modalityTranscribe:
		return "transcription"
	default:
		return "chat turns"
	}
}

// servesModality reports whether a resolved client implements the interface
// this modality needs, and names the miss when it does not.
//
// The INTERFACE is the authority, not the provider record's declared modality.
// A record says what it is for; the client says what it can do, and the client
// is what the call is about to be handed to.
func servesModality(client any, mod providerModality) (bool, string) {
	var ok bool
	switch mod {
	case modalityStreamChat:
		_, ok = client.(common.ChatStreamProvider)
	case modalityStreamTools:
		_, ok = client.(common.ChatStreamWithToolsProvider)
	case modalityTools:
		_, ok = client.(common.ToolCallingChatAIProvider)
	case modalityStructured:
		_, ok = client.(common.ChatStructuredProvider)
	case modalityVision:
		_, ok = client.(common.VisionAIProvider)
	case modalityEmbedding:
		_, ok = client.(memql.EmbeddingAIProvider)
	case modalitySpeech:
		_, ok = client.(memql.SpeechAIProvider)
	case modalityTranscribe:
		_, ok = client.(memql.TranscriptionAIProvider)
	default:
		_, ok = client.(common.ChatAIProvider)
	}
	if ok {
		return true, ""
	}
	return false, "does not serve " + modalityName(mod)
}

// chainWinner is the entry a walk settled on, plus the concrete names left
// after it for the fallback wrapper to try.
type chainWinner struct {
	entry *memql.ProviderConfigEntry
	door  string
	// client OVERRIDES the entry's own client, and is set only for a SESSION
	// winner (design D7): the step is handed to an app session, and the thing
	// that carries it is not the registry entry's appProvider.
	client    any
	remaining []string
}

// resolveChain decides which provider serves this call, and records the whole
// decision on the way (epic memql#5127, design D5 / D9 / D10).
//
// The order:
//
//  1. An EXPLICIT PROVIDER wins. No rule is consulted and no level is
//     overridden: a pin is a caller saying "this one" about a specific call,
//     and a rule that could override it would make the pin advisory.
//  2. Otherwise the first matching RULE decides. Its @level overrides the
//     call's -- both are recorded -- its @policy names the chain, and its
//     @exclude entries come out of that chain before anything is tried.
//  3. The chain is walked at the effective level, with a reason recorded for
//     every entry passed over.
//  4. On exhaustion the rule says what happens: DEGRADE walks again at the
//     next level down, re-matching the rule there because a different rule may
//     name a different policy, and records servedLevel and degraded; PARK
//     returns the refusal with the report. `fast` is the floor, and
//     `embeddings` never degrades at all.
//  5. CLOUD CONSENT is the last thing checked, after every door is shut,
//     because it is a person's decision about a situation they were shown.
func (r *Router) resolveChain(ctx context.Context, req ResolveRequest, mod providerModality) ([]string, Resolved, error) {
	if err := ctx.Err(); err != nil {
		return nil, Resolved{}, err
	}
	var policies *memql.PolicyRegistry
	var rules *memql.RuleRegistry
	if r != nil && r.policies != nil {
		var err error
		policies, rules, err = configuredRouting(ctx, r.policies, r.rules)
		if err != nil {
			return nil, Resolved{}, err
		}
	}
	report := &doorReporter{}

	// NO RULES MEANS NO ROUTING. See New: the only alternative to refusing is
	// the router picking a chain on its own, which is the precedence this epic
	// deleted, arriving back silently.
	if r == nil || r.rules == nil {
		report.note("(rules)", "no rule registry is wired into this router, so no rule can decide this call")
		return nil, Resolved{}, r.refusalWith(report, work.RefusalEveryDoorShut, "", "", airoute.Decision{
			RequestedLevel: req.Level,
			Level:          req.Level,
			ServedLevel:    req.Level,
		})
	}

	if explicit := strings.TrimSpace(req.ExplicitProvider); explicit != "" {
		winner, err := r.walkChain(ctx, req, mod, []string{explicit}, nil, report, "")
		if err != nil {
			return nil, Resolved{}, err
		}
		decision := airoute.Decision{
			RequestedLevel:   req.Level,
			Level:            req.Level,
			ServedLevel:      req.Level,
			Touches:          req.Touches,
			MinContextTokens: req.Needs.MinContextTokens,
		}
		if winner != nil {
			return chainAndResolved(r.resolvedFrom(winner, mod, "", report, decision))
		}
		resolved, err := r.consentOrRefusal(ctx, req, mod, report, "", decision)
		if err != nil {
			return nil, Resolved{}, err
		}
		return chainAndResolved(resolved)
	}

	// THE DEGRADE LOOP. Each pass matches a rule AT THE CURRENT LEVEL, because
	// a different rule may name a different policy there -- which is the only
	// way a second walk can produce a different answer at all.
	level := req.Level
	served := req.Level
	var firstRule, lastRule *memql.RuleConfig
	// walked fingerprints the chains already exhausted. A degrade that
	// re-matches to the SAME chain cannot produce a different answer, and
	// walking it again would fill the report with a second copy of reasons the
	// reader has already read.
	walked := map[string]bool{}
	// seenLevels stops a cycle. A rule may RAISE the level it serves at, so
	// "degrade from what was served" can arrive back at a level already tried;
	// without this the loop spins forever on a pair of rules that point at
	// each other, which is a configuration an owner can write.
	seenLevels := map[airoute.Level]bool{}

	for !seenLevels[level] {
		seenLevels[level] = true

		levelReq := req
		levelReq.Level = level
		rule := matchRuleFrom(rules, levelReq)
		if rule == nil {
			report.note("(rules)", "no rule matched this call and no default rule is registered")
			break
		}
		if firstRule == nil {
			firstRule = rule
		}
		lastRule = rule
		effective := rule.EffectiveLevel(level)
		served = effective

		chain, removed, ok := chainForPolicies(policies, rule, report)
		if !ok {
			break
		}

		if key := strings.Join(chain, "\x00"); walked[key] {
			report.noteConsidered("(rule "+rule.Name+")", "",
				"resolves the chain already exhausted at a higher level, so it is not walked again")
		} else {
			walked[key] = true
			// The excludes are noted HERE rather than where they are applied,
			// so a degrade that re-matches to the same chain does not put a
			// second copy of every exclusion on the record.
			for _, e := range removed {
				report.noteConsidered(e, doorFor(e), "excluded by rule "+rule.Name)
			}
			winner, err := r.walkChain(ctx, levelReq, mod, chain, rule.Excludes, report, rule.Policy)
			if err != nil {
				return nil, Resolved{}, err
			}
			if winner != nil {
				// Level is what the call asked for once the FIRST matching
				// rule had its say. A rule raising or lowering the level is
				// the routing decision rather than a degradation, so Degraded
				// compares what SERVED against this and not against what the
				// call declared.
				asked := firstRule.EffectiveLevel(req.Level)
				return chainAndResolved(r.resolvedFrom(winner, mod, rule.Policy, report, airoute.Decision{
					RequestedLevel:   req.Level,
					Level:            asked,
					ServedLevel:      effective,
					Degraded:         effective != asked,
					Rule:             rule.Name,
					Policy:           rule.Policy,
					Touches:          req.Touches,
					MinContextTokens: req.Needs.MinContextTokens,
				}))
			}
		}

		// PARK IS THE RULE'S ANSWER and it stops the walk here. A degraded
		// reasoning call returns a confident answer from a model that could
		// not do the work, and nothing downstream can tell the difference.
		if rule.Parks() {
			report.noteConsidered("(rule "+rule.Name+")", "",
				"declares onUnavailable=park, so the call is refused rather than degraded")
			break
		}
		next, canDegrade := effective.Degrade()
		if !canDegrade {
			report.noteConsidered("(level "+string(effective)+")", "", degradeFloorReason(effective))
			break
		}
		level = next
	}

	decision := airoute.Decision{
		RequestedLevel:   req.Level,
		Level:            req.Level,
		ServedLevel:      served,
		Touches:          req.Touches,
		MinContextTokens: req.Needs.MinContextTokens,
	}
	policyName := ""
	if firstRule != nil {
		decision.Level = firstRule.EffectiveLevel(req.Level)
	}
	if lastRule != nil {
		decision.Rule = lastRule.Name
		decision.Policy = lastRule.Policy
		policyName = lastRule.Policy
	}
	resolved, err := r.consentOrRefusal(ctx, req, mod, report, policyName, decision)
	if err != nil {
		return nil, Resolved{}, err
	}
	return chainAndResolved(resolved)
}

// chainAndResolved is the three-value form resolveChain returns: the concrete
// chain the fallback wrapper walks, the resolution beside it, no error.
func chainAndResolved(resolved Resolved) ([]string, Resolved, error) {
	return resolved.Chain, resolved, nil
}

// degradeFloorReason says WHY a level has nowhere to go, because the two
// reasons are different answers to an operator.
func degradeFloorReason(level airoute.Level) string {
	if level == airoute.LevelEmbeddings {
		return "embeddings never degrades: a degraded embedder answers in a different vector space, " +
			"so the vector would not belong in the index it was about to be written to"
	}
	return "there is no level below " + string(level) + " to degrade to"
}

// chainFor resolves a rule's policy into the chain to walk, with the rule's
// excludes removed.
//
// A chain that empties out is returned EMPTY and walked as such. The rule's
// onUnavailable then decides, which is the answer an operator asked for when
// they excluded every entry -- falling back to the unexcluded chain would
// quietly undo the exclusion they wrote.
func (r *Router) chainFor(rule *memql.RuleConfig, report *doorReporter) (chain, removed []string, ok bool) {
	return chainForPolicies(r.policies, rule, report)
}

func chainForPolicies(policies *memql.PolicyRegistry, rule *memql.RuleConfig, report *doorReporter) (chain, removed []string, ok bool) {
	if policies == nil {
		report.note("(policy "+rule.Policy+")", "no policy registry is wired into this router")
		return nil, nil, false
	}
	policy, found := policies.Lookup(rule.Policy)
	if !found {
		report.note("(policy "+rule.Policy+")", "rule "+rule.Name+" names a policy that is not registered")
		return nil, nil, false
	}
	chain, removed = applyExcludes(policy.ProviderChain(), rule.Excludes)
	return chain, removed, true
}

// walkChain tries the entries of one chain in order, at one level.
//
// It returns (nil, nil) when the chain is exhausted -- an answer, not a fault
// -- and a non-nil error only for a HARD stop: the cost ceiling, or a
// `policy:` reference that should have been expanded at load. A ceiling
// refusal does not degrade, because the ceiling is a condition only a person
// changes and the next level down would meet it identically.
func (r *Router) walkChain(
	ctx context.Context,
	req ResolveRequest,
	mod providerModality,
	chain []string,
	excludes []string,
	report *doorReporter,
	policyName string,
) (*chainWinner, error) {
	sawLocalDoor := false
	// EXCLUDES ARE APPLIED TWICE, AND BOTH ARE THE POINT. chainFor removed the
	// entries an author wrote (`app:*`, `fleet:strongest`); this removes the
	// CONCRETE names a selector expanded into, which is the form the demotion
	// vehicle of epic 4 actually takes -- `@exclude("fleet:qwen3.5:7b")` names
	// a model, and no chain entry is ever spelled that way.
	banned := bannedSet(excludes)

	for idx, rawEntry := range chain {
		entryDoor := doorFor(rawEntry)

		// THE FEDERATION HOP ASKS THE GUARD FIRST, and only when a LOCAL
		// door preceded it in the chain. That condition is the whole
		// distinction between the default three-step chain and a policy an
		// operator wrote to reach a vendor directly: the ceiling governs
		// falling BACK to paid inference, not choosing it. A chain that
		// starts at a vendor is a decision somebody made, and refusing it
		// here would break every cloud-quality policy in the tree.
		//
		// It is asked of the ENTRY an author wrote rather than of the
		// candidates it expands into, and that placement is load-bearing now
		// that entries expand: a fleet selector with nothing eligible produces
		// NO candidates, so a per-candidate flag would leave sawLocalDoor
		// false and let the next hop skip the ceiling entirely -- a silent
		// paid call at exactly the moment the local door was shut.
		if entryDoor == DoorFederation && sawLocalDoor {
			if reason, reached := r.ceilingReached(ctx); reached {
				report.note(rawEntry, "the cost ceiling for this process has been reached")
				return nil, r.refusalWith(report, work.RefusalCeilingReached, policyName, reason, airoute.Decision{
					RequestedLevel: req.Level,
					Level:          req.Level,
					ServedLevel:    req.Level,
					Policy:         policyName,
					Touches:        req.Touches,
				})
			}
		}
		if entryDoor == DoorLocal || entryDoor == DoorApp {
			sawLocalDoor = true
		}

		candidates, err := r.expandEntry(ctx, req, rawEntry, report)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			return nil, err
		}
		for ci, cand := range candidates {
			if banned[cand.Name] {
				report.note(cand.Name, "excluded by the rule that chose this chain")
				continue
			}
			// EntryForUser, not Entry: a `fleet:` or `app:` entry resolves
			// against the ACTING USER'S machines (epic memql#4676), and
			// resolving it against the system catalog instead would report a
			// live laptop as unavailable -- which, with a fallback, is a
			// silent paid call for a user whose machine was awake the whole
			// time.
			entry, ok := r.providers.EntryForUser(ctx, req.UserId, cand.Name)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !ok {
				report.note(cand.Name, "no provider by that name is registered")
				continue
			}
			if !entry.Available {
				reason := "unavailable"
				if err := entry.Err(); err != nil {
					reason = err.Error()
				}
				report.noteLocal(cand.Name, reason, r.consideredFor(ctx, req.UserId, cand.Name))
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				continue
			}
			// The context floor is checked HERE only for a federation record.
			// A fleet model's window was already checked against the same
			// floor by the catalog, against the machine's own report, and a
			// synthesized fleet or app entry carries no contextWindow param --
			// so asking one would report every local model as undeclared.
			if cand.Door == DoorFederation {
				if miss := contextFloorMiss(entry.Config, req.Needs.MinContextTokens); miss != "" {
					report.note(cand.Name, miss)
					continue
				}
			}
			// Confirm interface support for the requested modality.
			//
			// An app door lands here for a TOOL turn and is NOT passed over
			// any more (epic memql#5391, design D7). On a tool turn MemQL is
			// driving and an app is an agent that drives itself, so the app
			// cannot serve the TURN -- it takes the whole STEP instead, which
			// is the `session` door. It still reaches MemQL's tools through
			// MCP, in the other direction.
			//
			// The decision lives in session_door.go because providerLookup has
			// to reach the same answer when the fallback wrapper re-resolves
			// this winner by name.
			if serves, why := servesModality(entry.Client, mod); !serves {
				sessionClient, isSession, sessionErr := r.sessionDoorFor(req, cand.Name, mod)
				if sessionErr != nil {
					return nil, sessionErr
				}
				if isSession {
					// NO REMAINING CHAIN, and that is the park rule (design D7,
					// carried from the 2026-09-06 record). A session that fails
					// has run on somebody's machine; letting the fallback
					// wrapper advance to the vendor behind it would be a silent
					// paid call at exactly the moment the local door shut. A
					// machine that went to sleep parks the step with the
					// existing inferenceUnavailable approval instead, which is
					// a condition the world changes.
					return &chainWinner{entry: entry, door: DoorSession, client: sessionClient}, nil
				}
				report.note(cand.Name, why)
				continue
			}
			return &chainWinner{
				entry:     entry,
				door:      cand.Door,
				remaining: r.remainingNames(ctx, req, candidates[ci:], chain[idx+1:], banned),
			}, nil
		}
	}
	return nil, nil
}

// remainingNames is the concrete chain the fallback wrapper walks: the winner,
// then the candidates after it inside the same entry, then everything the
// later entries expand to.
//
// The tail is expanded HERE rather than left as the entries an author wrote,
// because the wrapper resolves by NAME and `federation:cheapest` is not a name
// -- leaving it would silently drop the whole federation step on a pre-flight
// error. The expansion writes to a THROWAWAY report: these entries were never
// tried, and a reason recorded for an entry nobody reached is a line that
// contradicts the walk it appears in.
func (r *Router) remainingNames(
	ctx context.Context,
	req ResolveRequest,
	rest []candidate,
	laterEntries []string,
	banned map[string]bool,
) []string {
	out := make([]string, 0, len(rest))
	for _, c := range rest {
		if !banned[c.Name] {
			out = append(out, c.Name)
		}
	}
	discard := &doorReporter{}
	for _, entry := range laterEntries {
		cands, err := r.expandEntry(ctx, req, entry, discard)
		if err != nil {
			continue
		}
		for _, c := range cands {
			if !banned[c.Name] {
				out = append(out, c.Name)
			}
		}
	}
	return out
}

// resolvedFrom assembles the answer, KEEPING THE DOOR REPORT ON SUCCESS.
//
// That is design D10 and it is the whole point of the report: a rule is
// falsifiable only if the decisions it made can be read, and what a chain did
// not pick is half of that. The pre-rules walk accumulated exactly this and
// dropped it with the stack frame the moment an entry won.
func (r *Router) resolvedFrom(
	winner *chainWinner,
	mod providerModality,
	policyName string,
	report *doorReporter,
	decision airoute.Decision,
) Resolved {
	decision.Door = winner.door
	decision.Considered = append(report.entries(), airoute.ConsideredEntry{
		Entry:  winner.entry.Config.Name,
		Door:   winner.door,
		Reason: "selected",
	})
	decision.Outcome = airoute.OutcomeOK
	// A SESSION WINNER'S CHAIN IS ITSELF AND NOTHING ELSE. `remaining` is empty
	// for one by construction (see walkChain), and naming the winner keeps the
	// fallback wrapper able to resolve it -- while giving it nowhere to advance
	// to, which is the park rule.
	chain := winner.remaining
	if winner.door == DoorSession {
		chain = []string{winner.entry.Config.Name}
	}
	return Resolved{
		ProviderName: winner.entry.Config.Name,
		Vendor:       vendorFromType(winner.entry.Config.Type),
		Model:        winner.entry.Config.Model,
		Pricing:      winner.entry.Config.Pricing(),
		Streaming:    mod == modalityStreamTools || mod == modalityStreamChat,
		PolicyName:   policyName,
		Chain:        chain,
		Decision:     decision,
		Entry:        winner.entry,
		Client:       winner.client,
	}
}

// consentOrRefusal is the last step: every door is shut, so either the person
// already said yes to a paid provider for THIS call, or the call is refused.
//
// EVERY DOOR IS SHUT. Reaching here is itself the proof that no paid fallback
// was authored past the ones already tried: had one been in the chain and been
// available, it would have been returned above. That is what makes the
// no-silent-spend property structural rather than a rule somebody has to
// remember -- there is no branch here that could choose a provider the policy
// never mentioned.
//
// The one exception is a person's decision rather than a code path: the caller
// carries explicit consent for THIS call. The surface that set it showed the
// refusal first and got a yes; nothing here can set it.
func (r *Router) consentOrRefusal(
	ctx context.Context,
	req ResolveRequest,
	mod providerModality,
	report *doorReporter,
	policyName string,
	decision airoute.Decision,
) (Resolved, error) {
	if err := ctx.Err(); err != nil {
		return Resolved{}, err
	}
	if req.CloudConsent {
		if client, resolved, ok := r.consentedCloudFallback(ctx, req, mod); ok {
			r.logger.Info("router: every door was shut and the user consented to a paid provider for this call",
				"provider", resolved.ProviderName, "policy", policyName)
			_ = client
			decision.Door = DoorFederation
			decision.Considered = append(report.entries(), airoute.ConsideredEntry{
				Entry:  resolved.ProviderName,
				Door:   DoorFederation,
				Reason: "selected: the caller carried explicit consent to reach a paid provider for this call",
			})
			decision.Outcome = airoute.OutcomeOK
			resolved.PolicyName = policyName
			resolved.Chain = []string{resolved.ProviderName}
			resolved.Decision = decision
			return resolved, nil
		}
		report.note("(consent)", "a paid provider was approved for this call, but this cluster has none configured")
	}
	return Resolved{}, r.refusalWith(report, work.RefusalEveryDoorShut, policyName, "", decision)
}

// refusalWith builds the typed refusal and stamps the decision onto it, so a
// park is as legible as a hit (design D9).
func (r *Router) refusalWith(
	report *doorReporter,
	code, policyName, ceilingReason string,
	decision airoute.Decision,
) error {
	refusal := report.refusal(code, policyName, ceilingReason)
	decision.Considered = report.entries()
	decision.Outcome = code
	refusal.Decision = decision
	return refusal
}

// providerLookup resolves a chain entry by name into its client +
// metadata + interface check for the requested modality. Returns
// (nil, zero, false) when the provider is unregistered, unavailable,
// or doesn't implement the interface.
func (r *Router) providerLookup(ctx context.Context, req ResolveRequest, name string, mod providerModality) (any, Resolved, bool) {
	entry, ok := r.providers.EntryForUser(ctx, req.UserId, name)
	if !ok || !entry.Available || entry.Client == nil {
		return nil, Resolved{}, false
	}
	resolved := Resolved{
		ProviderName: entry.Config.Name,
		Vendor:       vendorFromType(entry.Config.Type),
		Model:        entry.Config.Model,
		Pricing:      entry.Config.Pricing(),
		Streaming:    mod == modalityStreamTools || mod == modalityStreamChat,
		Entry:        entry,
	}
	// THE ATTEMPT'S DOOR IS ESTABLISHED HERE, and withDecisionFrom carries it
	// rather than re-deriving it from the name -- since epic memql#5391 the
	// name cannot say which door was taken, because `app:claude-code` serves a
	// chat turn as `app` and takes a whole step as a `session`.
	resolved.Decision.Door = doorFor(entry.Config.Name)
	if ok, _ := servesModality(entry.Client, mod); !ok {
		// THE SAME QUESTION THE WALK ASKED, and asking it here is what keeps
		// the fallback wrapper from stepping past a session winner it just
		// re-resolved by name (design D7). Without it the wrapper would skip
		// the app entry as "does not serve tool-calling turns" and advance to
		// whatever is behind it, which on the shipped chains is a vendor.
		//
		// THE STEPLESS ERROR IS SKIPPED RATHER THAN SURFACED, and that is
		// sound rather than a shortcut: this lookup has no error channel, and
		// the case cannot arrive. A stepless call is refused by the WALK,
		// before any wrapper is built -- so the only way here is a chain the
		// walk already resolved, which means the request carried a step. If it
		// ever did arrive, a session winner's chain is itself alone, so the
		// skip exhausts the chain and refuses rather than reaching a vendor.
		sessionClient, isSession, err := r.sessionDoorFor(req, name, mod)
		if err != nil || !isSession {
			return nil, Resolved{}, false
		}
		resolved.Client = sessionClient
		resolved.Decision.Door = DoorSession
		return sessionClient, resolved, true
	}
	var client any = entry.Client
	// Bind the floor on every lookup, including fallback attempts and direct
	// structured/vision resolutions. The provider returns an independent client.
	if scoped, ok := client.(interface{ WithMinContextTokens(int) any }); ok {
		client = scoped.WithMinContextTokens(req.Needs.MinContextTokens)
	}
	// Bind the LEVEL the same way (epic memql#5391, design D8). An app door
	// serving a chat turn opens a session too, and the cockpit translates the
	// level into that app's own knobs -- so a `fast` turn and a `reasoning`
	// one through the same signed-in app should not run identically.
	if levelled, ok := client.(interface{ WithLevel(string) any }); ok {
		client = levelled.WithLevel(string(req.Level))
	}
	return client, resolved, true
}

// buildRouterCallArgs assembles the recordRouterCall arg map for one
// call. callId is the row's own shortId -- a freshly-minted bare slug, NOT the
// requestId. The requestId is a fully-qualified utterance id
// (v1:cognition:utterance:<uuid>); using it as the v1:router:call shortId is
// rejected by canonical-id validation (one concept's full id can't be another
// concept's shortId), which silently dropped every ledger write (memql#1244).
// The originating utterance id is preserved on the requestId field for
// correlation; minting a fresh id (vs. extracting the bare UUID) also avoids
// an id collision when one request fans out to a fallback call.
func buildRouterCallArgs(rec CallRecord, callId string) map[string]any {
	args := map[string]any{
		"callId":             callId,
		"requestId":          rec.RequestId,
		"agentId":            rec.AgentId,
		"userId":             rec.UserId,
		"callerKind":         callerKindOrUnattributed(rec.CallerKind),
		"promptName":         rec.PromptName,
		"policyName":         rec.PolicyName,
		"vendor":             rec.Vendor,
		"model":              rec.Model,
		"providerName":       rec.ProviderName,
		"inputTokens":        rec.InputTokens,
		"outputTokens":       rec.OutputTokens,
		"cachedInputTokens":  rec.CachedInputTokens,
		"tokensEstimated":    rec.TokensEstimated,
		"inputCost":          rec.InputCost,
		"outputCost":         rec.OutputCost,
		"cachedInputCost":    rec.CachedInputCost,
		"totalCost":          rec.TotalCost,
		"pricingConfigured":  rec.PricingConfigured,
		"timeToFirstTokenMs": rec.TimeToFirstTokenMs,
		"totalDurationMs":    rec.TotalDurationMs,
		"tokensPerSec":       rec.TokensPerSec,
		"streaming":          rec.Streaming,
		"outcome":            rec.Outcome,
		"errorCategory":      rec.ErrorCategory,
		"errorMessage":       rec.ErrorMessage,
		"fallbackFromModel":  rec.FallbackFromModel,
		"servedModel":        rec.ServedModel,
		"servedEffort":       rec.ServedEffort,
		"billing":            billingOrMetered(rec.Billing),
		"executionSurface":   rec.ExecutionSurface,

		// THE DECISION (epic memql#5127, design D10). Written on a hit AND on
		// a park, so a refusal is as legible as a resolution. `considered` is
		// the door report kept on SUCCESS as well -- what a chain did not pick
		// is half the decision, and before this it was accumulated and then
		// dropped with the stack frame the moment an entry won.
		"level":              rec.Level,
		"requestedLevel":     rec.RequestedLevel,
		"servedLevel":        rec.ServedLevel,
		"degraded":           rec.Degraded,
		"rule":               rec.Rule,
		"policy":             rec.Policy,
		"door":               rec.Door,
		"considered":         consideredArgs(rec.Considered),
		"touches":            rec.Touches,
		"minContextTokens":   rec.MinContextTokens,
		"machineOwnerUserId": rec.MachineOwnerUserId,
	}

	// `cacheKind` IS OMITTED RATHER THAN SENT EMPTY, and it has to be: the
	// mutation declares it as a two-value enum, and "" is not one of the two
	// -- sending it would fail argument validation and lose the WHOLE row for
	// every ordinary provider call. Absent is also the right thing to store:
	// on this concept an absent cacheKind means a provider answered, which is
	// what every row written before memql#5581 was.
	if airoute.ValidCacheKind(rec.CacheKind) {
		args["cacheKind"] = rec.CacheKind
	}
	return args
}

// consideredArgs renders the door report as the plain []map the mutation's
// []object argument takes. It returns an EMPTY SLICE rather than nil for an
// empty report, because nil and "the walk considered nothing" are different
// claims and only one of them is ever true: a resolution always considered at
// least the entry it took.
func consideredArgs(entries []airoute.ConsideredEntry) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"entry":  e.Entry,
			"door":   e.Door,
			"reason": e.Reason,
		})
	}
	return out
}

// billingOrMetered normalizes a record's billing for the ledger. An
// empty value reads as metered, which keeps every writer that predates
// memql#4362 meaning exactly what it did -- and is the conservative
// direction besides: unattributed spend counts against the dollar
// ceiling rather than disappearing into the covered bucket.
func billingOrMetered(billing string) string {
	switch billing {
	case BillingSubscription, BillingLocal, BillingUnknown:
		return billing
	}
	return BillingMetered
}

// callerKindOrUnattributed normalizes a record's caller kind for the ledger.
//
// A BLANK IS THE ONE ANSWER THE FIELD MUST NEVER CARRY (memql#5581): it is
// exactly the ambiguity the field exists to remove, and it would reappear on
// any path that built a CallRecord without going through ResolveFor. An
// unrecognised value is normalized the same way rather than stored: the five
// kinds are a closed set on the concept, so a sixth string would be refused at
// write time and lose the whole row.
func callerKindOrUnattributed(kind string) string {
	switch kind {
	case auth.CallerKindUser, auth.CallerKindSystem, auth.CallerKindConnector,
		auth.CallerKindAnonymous, auth.CallerKindUnattributed:
		return kind
	}
	return auth.CallerKindUnattributed
}

// LedgerQueueDepth is how many pending v1:router:call records the router holds
// before it starts dropping them (memql#5581).
//
// THE BOUND IS THE POINT. What it replaced was a goroutine per record: a node
// answering a burst of model calls spawned a burst of writers, each rendering
// a ~1.1 KB mutation, parsing it and taking a database connection, all at the
// moment the node was busiest -- the decision records competing with the work
// they describe. One writer drains this queue, so the ledger costs one
// goroutine and at most one concurrent write no matter how many calls are in
// flight.
//
// The depth is the same order as component/logstore's (4096 lines, batch 256),
// which is the tree's other bounded-queue-with-counted-drops sink, and it is
// deliberately generous: a full queue is a node whose database cannot keep up
// with its own model calls, and at that point dropping the record is the right
// answer and RecordsDropped is where it is reported.
const LedgerQueueDepth = 1024

// ledgerWriteTimeout bounds one ledger write. Unchanged from the per-record
// goroutine it replaced.
const ledgerWriteTimeout = 5 * time.Second

// recordCall queues one v1:router:call row for the ledger writer.
//
// NON-BLOCKING, ALWAYS. It is called from stream goroutines and from the
// observer's end-of-stream path, and a ledger that could block either of them
// would put observability on the reply's critical path. A queue that is full
// drops the record and counts it; it never waits.
//
// The write itself -- render, parse, mutation -- happens on the writer, which
// is the one place this router touches the graph for observability.
func (r *Router) recordCall(rec CallRecord) {
	// A router with no engine cannot write the ledger, and that is a state
	// RecordsDropped's own doc anticipates ("engine unavailability"). Without
	// this check the writer dereferences nil and takes the WHOLE PROCESS with
	// it -- a panic on a goroutine nobody recovers is fatal, so the failure
	// mode of an unwired observability path was a crash rather than a missing
	// row.
	if r == nil || r.engine == nil {
		if r != nil {
			r.recordsDropped.Add(1)
		}
		return
	}
	queue := r.ledgerQueue()
	select {
	case queue <- rec:
	default:
		r.recordsDropped.Add(1)
		r.logger.Warn("router: the ledger queue is full; dropping a v1:router:call row",
			"depth", LedgerQueueDepth,
			"requestId", rec.RequestId,
			"provider", rec.ProviderName,
			"outcome", rec.Outcome,
		)
	}
}

// ledgerQueue returns the queue, starting the single writer on first use.
//
// LAZY RATHER THAN IN New, because a Router built by hand -- a test, an
// embedding that skipped the constructor -- must not be left without one, and
// because a router that never records a call should not hold a goroutine.
func (r *Router) ledgerQueue() chan CallRecord {
	r.ledgerOnce.Do(func() {
		r.ledger = make(chan CallRecord, LedgerQueueDepth)
		go r.drainLedger(r.ledger)
	})
	return r.ledger
}

// drainLedger is the ONE writer. It runs for the life of the process and
// writes records in the order they were recorded.
//
// SERIAL ON PURPOSE. A pool would restore the contention this queue exists to
// remove, and the throughput a single writer gives -- one mutation per
// database round trip -- is far above the rate at which a node makes model
// calls. A node that does exceed it is telling the operator something, and the
// drop counter is how it says so.
func (r *Router) drainLedger(queue <-chan CallRecord) {
	for rec := range queue {
		r.writeRecord(rec)
	}
}

// writeRecord renders and executes one ledger mutation.
//
// The ledger mutation goes through `insert()` which requires an actor
// (see component/memql/executor.go:mutationActor). We stamp a
// synthetic "system:router" principal onto the detached context so
// the write succeeds -- the alternative was every call emitting a
// "no actor found in context" warning on every turn. The context is
// detached so the caller's cancellation never interrupts observability.
func (r *Router) writeRecord(rec CallRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), ledgerWriteTimeout)
	defer cancel()
	ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{
		Subject: "system:router",
		Claims:  map[string]any{"sub": "system:router"},
	})

	args := buildRouterCallArgs(rec, id.NewShortId())
	query, err := langparser.RenderCall("recordRouterCall", args)
	if err != nil {
		r.recordsDropped.Add(1)
		r.logger.Warn("router: rendering the record call failed", "error", err, "requestId", rec.RequestId)
		return
	}
	if _, err := r.engine.Execute(ctx, query); err != nil {
		r.recordsDropped.Add(1)
		r.logger.Warn("router: failed to write v1:router:call row",
			"error", err,
			"requestId", rec.RequestId,
			"provider", rec.ProviderName,
			"outcome", rec.Outcome,
		)
	}
}

// RecordsDropped returns the cumulative count of ledger writes the router
// has skipped: an unavailable engine, a marshal error, a failed write, or a
// full queue. Exposed for health endpoints and tests.
func (r *Router) RecordsDropped() uint64 {
	return r.recordsDropped.Load()
}

func (r *Router) stampRequestId(req ResolveRequest) ResolveRequest {
	if req.RequestId == "" {
		req.RequestId = id.NewShortId()
	}
	return req
}

// vendorFromType maps a provider .memql @type to a vendor family name.
// Kept inline rather than as a table so new vendors only need a case
// here when we add their client.
func vendorFromType(typeName string) string {
	lower := strings.ToLower(strings.TrimSpace(typeName))
	switch {
	case strings.Contains(lower, "openai"):
		return "openai"
	case strings.Contains(lower, "anthropic"):
		return "anthropic"
	default:
		return lower
	}
}

// firstNonEmpty returns the first non-empty trimmed string, or "".
// Used by callers that want to compose a precedence without nested ifs.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// consentedCloudPolicy is the DECLARED chain a one-shot cloud consent resolves
// through. It is a shipped, locked policy (epic memql#5127), so the answer to
// "what did I just consent to" is a thing in the tree a person can read.
const consentedCloudPolicy = "federationStrongest"

// consentedCloudFallback finds a paid provider to honour a one-shot consent.
//
// THE DECLARED CHAIN FIRST, THEN A DIRECT PICK, and the second half is the part
// that matters (epic memql#5137, D3).
//
// It used to read `providers.Default()`. That had to go: since every concrete
// record became a paid vendor model, "the registry default" meant whichever
// entry a map yielded first or whatever a Deployment manifest pinned -- a
// different decision from the one the person thought they were making, which is
// exactly the objection the old comment here raised and then failed to answer.
//
// Resolving through `federationStrongest` answers it: the person consented to
// cloud, and which cloud model that means is a policy they can read. But a
// policy is not sufficient on its own, because CONSENT EXISTS FOR THE CASE
// WHERE THE CHAIN ALREADY REFUSED. Making the one-shot human escape depend on
// the rule and policy corpus being loaded means a cluster whose corpus failed to
// load has no escape at all -- the person says yes and nothing happens, which is
// worse than the default it replaced.
//
// So: the policy when it is there, and a deterministic strongest-federated pick
// when it is not. The fallback is not a default -- nothing reaches it without an
// explicit human yes on this call -- which is the whole difference between it
// and what was deleted.
func (r *Router) consentedCloudFallback(ctx context.Context, req ResolveRequest, mod providerModality) (any, Resolved, bool) {
	if r == nil || r.providers == nil {
		return nil, Resolved{}, false
	}
	policies, _, err := configuredRouting(ctx, r.policies, r.rules)
	if err != nil {
		return nil, Resolved{}, false
	}
	for _, name := range r.consentedCloudChainFrom(policies) {
		if _, isFleet := memql.IsFleetReference(name); isFleet {
			// Consenting to cloud cannot resolve to a local model: the fleet
			// entry is exactly the one that was unavailable when the consent
			// was asked for.
			continue
		}
		if client, resolved, found := r.providerLookup(ctx, req, name, mod); found {
			return client, resolved, true
		}
	}
	return nil, Resolved{}, false
}

// consentedCloudChain is the order a consent tries federated providers in.
func (r *Router) consentedCloudChain() []string { return r.consentedCloudChainFrom(r.policies) }

func (r *Router) consentedCloudChainFrom(policies *memql.PolicyRegistry) []string {
	if policies != nil {
		if policy, ok := policies.Lookup(consentedCloudPolicy); ok {
			var chain []string
			for _, name := range policy.ProviderChain() {
				if n := strings.TrimSpace(name); n != "" {
					chain = append(chain, n)
				}
			}
			if len(chain) > 0 {
				return chain
			}
		}
	}
	// NO POLICY LOADED. Pick the strongest federated record directly, ordered
	// by declared context window and tie-broken by name so two replicas make
	// the same choice and a person can predict it.
	//
	// It is deliberately NOT map order, which is what the deleted default was.
	return r.providers.FederatedByStrength()
}

// Providers exposes the registry this router resolves against, so a caller
// that already holds a Router can ask about provider availability without
// being handed a second reference to keep in sync.
func (r *Router) Providers() *memql.ProviderRegistry {
	if r == nil {
		return nil
	}
	return r.providers
}

// consideredFor asks a LOCAL door for its own report: which machines or apps
// it looked at and why each was ruled out.
//
// It is asked only when the door did not open, and only of the doors that have
// such a report -- a vendor entry's unavailability is about a credential, not
// about a set of machines, and inventing a considered-list for it would put
// the same word in front of two different kinds of failure.
func (r *Router) consideredFor(ctx context.Context, userId, name string) map[string]string {
	if r == nil || r.providers == nil {
		return nil
	}
	if modelId, ok := memql.IsFleetReference(name); ok {
		if refusal := r.providers.FleetRefusal(ctx, userId, modelId); refusal != nil {
			return refusal.Considered
		}
		return nil
	}
	if appId, ok := memql.IsAppReference(name); ok {
		if refusal := r.providers.AppRefusal(ctx, userId, appId); refusal != nil {
			return refusal.Considered
		}
	}
	return nil
}

// ceilingReached asks the cost ceiling, through the injectable probe.
//
// A Router built by hand (a test, an embedding that skipped New) has no probe
// and answers NOT REACHED. That direction is the safe one here and it is the
// opposite of the usual fail-closed rule for a reason worth stating: a missing
// probe reporting "reached" would refuse every federation hop on any cluster
// whose router was not built through New, which turns an unwired dependency
// into a silent, total loss of paid inference.
func (r *Router) ceilingReached(ctx context.Context) (string, bool) {
	if r == nil || r.ceilingCheck == nil {
		return "", false
	}
	return r.ceilingCheck(ctx)
}

// Optional structural seam keeps this leaf module compatible with the
// published engine module while app/workspace builds install durable routing.
func configuredRouting(ctx context.Context, policies *memql.PolicyRegistry, rules *memql.RuleRegistry) (*memql.PolicyRegistry, *memql.RuleRegistry, error) {
	if source, ok := any(policies).(interface {
		SnapshotRouting(context.Context, *memql.RuleRegistry) (*memql.PolicyRegistry, *memql.RuleRegistry, error)
	}); ok && policies != nil {
		return source.SnapshotRouting(ctx, rules)
	}
	return policies, rules, nil
}
