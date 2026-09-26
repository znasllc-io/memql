package memql

// ai_resolver.go -- the seam this package reaches a model through
// (epic memql#5127, design D2).
//
// # Why a seam and not a call
//
// component/router is a separate Go module and it imports THIS package: it
// walks a chain over this package's ProviderRegistry and resolves fleet and
// app references through it. The dependency does not go back, and it cannot.
//
// Every call site design D2 re-points -- InvokeAI, InvokeAIStructured, the
// tool loop, the semantic cache -- lives here. So "each of them builds a
// router.ResolveRequest" is, taken literally, an import cycle. The vocabulary
// therefore lives in core/airoute, which both modules already require, and the
// engine holds an INTERFACE the router satisfies, installed from app/ where
// both are visible.
//
// # An unwired resolver refuses; it does not fall back
//
// This is the one property worth defending in review. A seam whose setter is
// never called is green, silent and inert -- the shape this tree has been
// bitten by before -- and the tempting nil-handling here is "fall back to the
// registry default", which is precisely the resolution path this epic deletes.
// It would reintroduce it on exactly the nodes where nobody wired the router,
// and nothing would say so: the calls would work, on a paid vendor model, and
// no decision record would exist to notice.
//
// So an unwired resolver returns ErrAIResolverUnwired, which reads as a
// configuration fault rather than as "no provider available" -- the two need
// different fixes and the sentences must not be confusable.

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/znasllc-io/memql/core/airoute"
)

// ErrAIResolverUnwired is returned when a model call is made on a node where
// nobody installed a resolver. It is a distinct error rather than a refusal
// code because the fix is in app wiring, not in a policy, a machine or a
// vendor account.
var ErrAIResolverUnwired = errors.New(
	"the AI resolver is not wired on this node: every model call is decided by the router seam, " +
		"and no router was installed. This is a boot-wiring fault, not an unavailable provider")

// ResolvedProvider is what the seam returns: the client to call, and the whole
// decision that chose it.
type ResolvedProvider struct {
	// Client is one of the common.* provider interfaces, matching the
	// request's Modality. It is `any` because the modalities do not share an
	// interface and inventing one here would put every provider shape in this
	// package's vocabulary; callers type-assert to the one they asked for.
	Client any

	// Resolution names what was picked and why.
	Resolution airoute.Resolution

	// Entry is the registry record behind Client, for the callers INSIDE this
	// package that need the provider's own configuration -- its answer-
	// affecting parameters for the model-call journal, its declared modality.
	//
	// It is on this type rather than on airoute.Resolution because it is a
	// provider RECORD, and putting one in the shared vocabulary would make
	// every module that names a level depend on how this package stores a
	// provider. Callers outside component/memql have no use for it and cannot
	// reach its unexported half anyway.
	Entry *ProviderConfigEntry
}

// AIResolver is the seam. component/router implements it.
type AIResolver interface {
	// ResolveFor picks a provider for the request and returns it with the
	// decision that led there. It returns an error the caller surfaces rather
	// than substituting a provider of its own -- there is no second-choice
	// path left in this package.
	ResolveFor(ctx context.Context, req airoute.ResolveRequest) (ResolvedProvider, error)
}

// aiResolverHolder is a separate value rather than a bare field so the setter
// and the reader share one lock. The resolver is installed once at boot and
// read on every AI call from every goroutine.
type aiResolverHolder struct {
	mu sync.RWMutex
	r  AIResolver
}

func (h *aiResolverHolder) set(r AIResolver) {
	h.mu.Lock()
	h.r = r
	h.mu.Unlock()
}

func (h *aiResolverHolder) get() AIResolver {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.r
}

// SetAIResolver installs the router as this engine's resolver. Called once
// from app/, on every node that makes AI calls.
func (e *MemQLEngine) SetAIResolver(r AIResolver) {
	if e == nil {
		return
	}
	e.aiResolver.set(r)
}

// HasAIResolver reports whether a resolver is installed. It exists for the
// boot check and for tests that assert the wiring happened; nothing on the
// call path branches on it, because a branch would be the fallback this file
// exists to refuse.
func (e *MemQLEngine) HasAIResolver() bool {
	return e != nil && e.aiResolver.get() != nil
}

// resolveAI is the ONE place this package reaches a provider.
//
// It validates the request before spending anything: a level outside the
// closed four and a modality outside the closed set are both authoring faults
// that would otherwise reach the rule matcher as a condition nothing can
// satisfy, and present as a call that mysteriously took the default rule.
func (e *MemQLEngine) resolveAI(ctx context.Context, req airoute.ResolveRequest) (ResolvedProvider, error) {
	if e == nil {
		return ResolvedProvider{}, ErrAIResolverUnwired
	}
	if !req.Level.Valid() {
		return ResolvedProvider{}, fmt.Errorf("AI call declares level %q: a level is one of %s", req.Level, airoute.LevelNames())
	}
	if !req.Modality.Valid() {
		return ResolvedProvider{}, fmt.Errorf("AI call declares modality %q, which the router does not derive", req.Modality)
	}
	// TEXT CALLS ALWAYS HAVE A FLOOR. A zero admits every entry and reads on the
	// decision record exactly like a floor that was measured and cleared, so a
	// caller that forgot to estimate gets the conservative default rather than
	// a silently unbounded request.
	if req.Modality == airoute.ModalitySpeech || req.Modality == airoute.ModalityTranscribe {
		// Audio runtimes advertise media limits, not a chat context window.
		req.Needs.MinContextTokens = 0
	} else if req.Needs.MinContextTokens <= 0 {
		req.Needs.MinContextTokens = airoute.EstimateMinContextTokens("", 0)
	}
	// ATTRIBUTION IS FILLED HERE TOO, not only in requestForPrompt
	// (memql#5581). This function is the one place this package reaches a
	// provider, so a Go call site with no prompt at all -- the gRPC chat and
	// suggest handlers, app/ai_call_sites.go, the compile pass -- gets the
	// same derivation without naming it. applyCallAttribution fills only what
	// is still empty, so the prompt path, which already ran it, is unchanged.
	req = applyCallAttribution(ctx, req)
	resolver := e.aiResolver.get()
	if resolver == nil {
		return ResolvedProvider{}, ErrAIResolverUnwired
	}
	return resolver.ResolveFor(ctx, req)
}

// ResolveAITyped resolves and type-asserts in one step, so a call site does
// not repeat the assertion and its failure message.
//
// A resolution whose client does not satisfy the caller's interface is a BUG
// rather than an unavailable provider: the router interface-checks the
// modality on the way in, so reaching here means the two disagree about what a
// modality means. It says so, rather than reporting the model as unavailable
// and sending somebody to look at their fleet.
//
// It is EXPORTED because most of the call sites are not in this package: the
// gRPC AI handlers live in component/grpc and the wiring closures live in
// app/, and both reach a model through this one function. Keeping it
// unexported would have left them the choice between a second seam and the
// direct accessors this epic fences off, which is the choice the fence exists
// to remove.
func ResolveAITyped[T any](ctx context.Context, e *MemQLEngine, req airoute.ResolveRequest) (T, airoute.Resolution, error) {
	var zero T
	got, err := e.resolveAI(ctx, req)
	if err != nil {
		return zero, airoute.Resolution{}, err
	}
	client, ok := got.Client.(T)
	if !ok {
		return zero, got.Resolution, fmt.Errorf(
			"router returned provider %q for modality %q and it does not serve that interface: "+
				"the router's modality check and this call site disagree about what %[2]q means",
			got.Resolution.ProviderName, req.Modality)
	}
	return client, got.Resolution, nil
}
