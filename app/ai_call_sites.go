package app

// ai_call_sites.go -- every model call app/ makes, as a named request
// (epic memql#5127, design D2).
//
// # Why the requests are functions rather than literals
//
// Each of these sites used to name a provider: `VisionProvider()`,
// `EmbeddingProvider(ctx, name)`, `StructuredChatProviderByName(ctx, name)`.
// A name at a call site is a release every time the fleet changes, it records
// no decision, and no rule an operator wrote can see it. Re-pointed, each site
// declares the LEVEL it needs and the MODALITY it derives, and the router
// picks.
//
// The declaration is only worth as much as its checkability, and a request
// built inline inside a closure is not checkable: a table test that asserts
// nothing because the requests are unreachable is worse than no table at all.
// So each site's request is a named function over its inputs, and
// call_site_levels_test.go is the table that pins the level and modality of
// every one of them.
//
// # Two of these resolve at BOOT, and that is deliberate
//
// The safety classifier and the healer are composed once during wiring, and
// their provider is chosen then. That is the shape they already had -- a nil
// provider left the layer off rather than failing the node -- and it is the
// honest one for a layer that is either present for the process's life or
// absent from it. The vision getter is the opposite: it is a CALLBACK,
// resolved per call, because a file uploaded an hour after boot must see the
// fleet as it is then.

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// visionResolveRequest is an image or a document handed to a model that can
// look at it: extracting text from a scan, describing a picture in a Library
// file.
//
// FAST, because this is extraction rather than reasoning -- the answer is fed
// to a chunker and an index, not read by a person. Needs.Vision is asserted
// even though the modality already implies it: the modality says which
// interface the client must satisfy, the need says what the MODEL must be able
// to do, and epic 3's local door reads the second when deciding whether a
// machine's model can serve the call.
//
// The context floor is left to the seam's default, and the reason is that
// nothing better is knowable here: this request picks a provider, and the
// bytes it will be shown arrive later, at Extract time.
func visionResolveRequest() airoute.ResolveRequest {
	return airoute.ResolveRequest{
		Level:    airoute.LevelFast,
		Modality: airoute.ModalityVision,
		Needs:    airoute.Needs{Vision: true},
	}
}

// embeddingResolveRequest is a vector embedding.
//
// LEVEL EMBEDDINGS, which is the one level that never degrades: a degraded
// embedder answers in a different vector space, so the result is not a worse
// vector but one that does not belong in the index it is about to be written
// to, and nothing downstream can tell the difference.
//
// The caller-supplied name is a PIN, not a lookup -- it rides ExplicitProvider
// and wins over every rule, while still passing through the doors and the
// ledger. The pinned embedding model id on a binding stays exactly as it is
// this epic; epic memql#5137 owns re-pointing it, so nothing changes on the
// wire here.
func embeddingResolveRequest(name string) airoute.ResolveRequest {
	return airoute.ResolveRequest{
		Level:            airoute.LevelEmbeddings,
		Modality:         airoute.ModalityEmbedding,
		ExplicitProvider: name,
	}
}

// safetyClassifierResolveRequest is the LLM half of the command-safety
// classifier: a short command, a fixed schema, a verdict.
//
// FAST -- it runs in front of a dispatch a person is waiting on, and it is
// triage. Needs.Structured is asserted rather than left to the modality
// because the schema is the whole point: a provider that answers prose here
// produces a parse failure that reads as a model problem.
//
// MEMQL_SAFETY_LLM_PROVIDER rides ExplicitProvider when it is set. The env var
// is what turns this layer on at all, so an empty name never reaches here.
func safetyClassifierResolveRequest(providerName string) airoute.ResolveRequest {
	return airoute.ResolveRequest{
		Level:            airoute.LevelFast,
		Modality:         airoute.ModalityStructured,
		Needs:            airoute.Needs{Structured: true},
		ExplicitProvider: providerName,
	}
}

// healingPatchResolveRequest is the healer's patch proposal: a precondition
// missed, and four typed patches offered to a person as a planReview.
//
// FAST, and the reason is worth stating because `reasoning` is the tempting
// answer. The proposal is not authored here -- it is a bounded set of typed
// patches over a template that already exists, and a person approves it before
// anything changes (design D5). Nothing is emitted or repaired on this call.
//
// It names NO provider. The site it replaced resolved
// `StructuredChatProviderByName(ctx, DefaultProviderName())` -- the registry's
// default, dressed as a choice -- and that default is precisely what the rules
// now decide.
func healingPatchResolveRequest() airoute.ResolveRequest {
	return airoute.ResolveRequest{
		Level:    airoute.LevelFast,
		Modality: airoute.ModalityStructured,
		Needs:    airoute.Needs{Structured: true},
	}
}

// callerVision is a vision provider that is RESOLVED WHEN IT IS CALLED, for
// whoever is making that call.
//
// WHY IT EXISTS. An extractor is built once, when the node starts or a plug-in
// is wired, and used for every upload after. Handing it a provider resolved at
// that moment resolved it for NOBODY: no caller, so the router judged it the
// cluster's own work, and the cluster's vendor account then described every
// person's pictures, whoever uploaded them. The cluster's account pays for a
// person's calls only when that person may use it (owner decision 2026-11-22,
// component/memql/vendor_account_cluster.go), and a picture follows the rule.
//
// So the provider an extractor holds is this one. Each CallVision resolves
// through the router with THAT call's context, which is where the caller is
// (applyCallAttribution reads it), and the gates in front of a vendor are
// asked about that person. A person no vision model can serve gets an error on
// the image, which the processor reports on that file; every other format is
// unaffected.
//
// Regression tests: vision_for_caller_test.go.
type callerVision struct {
	resolve func(ctx context.Context) common.VisionAIProvider
}

func (v callerVision) CallVision(ctx context.Context, prompt string, images []common.VisionContent) (string, error) {
	var provider common.VisionAIProvider
	if v.resolve != nil {
		provider = v.resolve(ctx)
	}
	if provider == nil {
		return "", fmt.Errorf("no vision model can describe this image for the caller")
	}
	return provider.CallVision(ctx, prompt, images)
}

// visionForCaller is the vision provider the app hands to every extractor.
func (a *App) visionForCaller() common.VisionAIProvider {
	return callerVision{resolve: a.resolveVisionProvider}
}

// resolveVisionProvider returns a vision-capable provider for the caller on
// ctx, or nil when no source can serve them. It is asked per call
// (callerVision), never once at start.
//
// NIL RATHER THAN AN ERROR, because that is the contract both callers already
// have: component/fileprocessor takes a provider that may be absent and
// degrades to the non-image formats it can read without one, reporting the
// absence on the image path where a person can see which file it affected.
// Returning an error here would have to be swallowed at both call sites
// anyway. The refusal is logged, so it is not silent.
func (a *App) resolveVisionProvider(ctx context.Context) common.VisionAIProvider {
	if a == nil || a.engine == nil {
		return nil
	}
	provider, _, err := memql.ResolveAITyped[common.VisionAIProvider](ctx, a.engine, visionResolveRequest())
	if err != nil {
		a.Logger.Warn("no vision model can serve this caller, so this image is not described; text formats are unaffected",
			"component", "ai.router", "error", err)
		return nil
	}
	return provider
}
