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
//
// Both boot-resolved layers are the CLUSTER'S OWN JOBS, and say so at the
// resolution and on every call (resolveForClusterJob, below): a call that
// names nobody is refused the cluster's vendor account since the owner's
// decision of 2026-11-23.

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/auth"
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
//
// indexer, when set, FIXES whom the call is resolved for: memory search
// embeds as its indexer, whoever asks (memql.EmbeddingIndexerForCall). It
// rides UserId, which the engine's attribution fills only when it is empty
// (component/memql ai_attribution.go) and the router reads to choose machines,
// so a fleet model is resolved against the indexer's machines and the decision
// names the indexer. Empty leaves the call its caller's, as every other site.
func embeddingResolveRequest(name, indexer string) airoute.ResolveRequest {
	return airoute.ResolveRequest{
		Level:            airoute.LevelEmbeddings,
		Modality:         airoute.ModalityEmbedding,
		ExplicitProvider: name,
		UserId:           indexer,
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

// The cluster's own jobs among these call sites, by the name each claims its
// work under (auth.ContextForClusterWork).
const (
	safetyClassifierJob = "safety-classifier"
	workHealerJob       = "work-healer"
)

// resolveForClusterJob resolves a structured client for one of the cluster's
// OWN JOBS, and hands back a client every call of which is that job's.
//
// WHY BOTH HALVES SAY IT (owner decision 2026-11-23). The cluster's vendor
// account pays for the cluster's own work without asking anybody, and only a
// call that SAYS it is the cluster's counts as such: a call that names nobody
// is refused the account (component/memql vendor_account_cluster.go). The
// safety check and the healer resolve their model once, when the node starts,
// with no caller, and serve every later caller:
//
//   - THE RESOLUTION is made under the job's name, or it would name nobody and
//     a vendor model would be passed over at start, leaving the layer off on a
//     cluster whose only model is its vendor account.
//   - EVERY CALL carries the job's name too. The router judges the account
//     again when a call is made, from that call's context
//     (component/router accounts.go, accountAtCall), and the context is the
//     caller's: the person whose command the safety check screens. Judged as
//     theirs, the check would be paid by them, or refused for them; it is the
//     cluster's work, which the owner's rule pays for as such.
//
// The job's name, not the system actor: ContextWithSystemActor would put a
// `system:` id on the call, and the fleet reads an acting id to choose
// machines -- a blank one reaches the machines owners lent to cluster work, a
// named one only that name's own -- so the job would lose the shared machines
// it reaches today. The name claims the work and leaves the id blank.
//
// resolve is the router seam (memql.ResolveAITyped in production); a test
// hands its own. A resolution error comes back as it is, and the layer the
// caller builds stays off, as it always did.
//
// Regression tests: cluster_jobs_test.go.
func resolveForClusterJob(
	job string,
	req airoute.ResolveRequest,
	resolve func(context.Context, airoute.ResolveRequest) (common.ChatStructuredProvider, error),
) (common.ChatStructuredProvider, error) {
	provider, err := resolve(auth.ContextForClusterWork(context.Background(), job), req)
	if err != nil {
		return nil, err
	}
	return clusterJobStructured{inner: provider, job: job}, nil
}

// clusterJobStructured is a structured client every call of which is one of
// the cluster's own jobs. The call's context is otherwise left as its caller
// made it: the caller's deadline, cancellation and identity ride along, and
// the identity still decides what the call records about who asked.
type clusterJobStructured struct {
	inner common.ChatStructuredProvider
	job   string
}

func (c clusterJobStructured) CallChatStructured(ctx context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, error) {
	return c.inner.CallChatStructured(auth.ContextForClusterWork(ctx, c.job), messages, schema)
}

// routerStructured is resolveForClusterJob's production seam.
func (a *App) routerStructured(ctx context.Context, req airoute.ResolveRequest) (common.ChatStructuredProvider, error) {
	provider, _, err := memql.ResolveAITyped[common.ChatStructuredProvider](ctx, a.engine, req)
	return provider, err
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

// resolveEmbeddingProvider is PluginContext.ResolveEmbeddingProvider: every
// embedding a plug-in makes -- an item indexed, a search query, a knowledge
// chunk, a seed -- is resolved here.
//
// MEMORY SEARCH EMBEDS AS ITS INDEXER, WHOEVER ASKS (design record
// 2026-11-23, section 1; component/memql embedder_indexer.go). The index is
// one for the whole cluster and the owner provides it, so the bound fleet
// model is resolved for the binding's indexer -- or for the indexer the
// context names, which is how the index job fills a model that is not active
// yet -- and the call then runs on their machines and the ones lent to them.
// Before, it was resolved for the caller, so an item written by somebody with
// no machine offering the model was never indexed and they could not search.
//
// Two halves, because the router can do only one of them. The REQUEST names
// the indexer (embeddingResolveRequest), which is what chooses their machines
// and puts them on the decision. The PROVIDER carries the indexer's assertion
// on each call (memql.EmbedAsIndexer), which is what the replica holding a
// machine's stream checks the machine against; the bff embeds memory and holds
// no stream, so without it every such call would be refused at the hop.
//
// A vendor model and a fleet model that is not memory search's are resolved
// for their caller exactly as before, and nothing is wrapped.
//
// Regression tests: embedding_indexer_test.go.
func (a *App) resolveEmbeddingProvider(ctx context.Context, name string) (memql.EmbeddingAIProvider, error) {
	indexer, err := memql.EmbeddingIndexerForCall(ctx, name)
	if err != nil {
		return nil, err
	}
	provider, _, err := memql.ResolveAITyped[memql.EmbeddingAIProvider](ctx, a.engine, embeddingResolveRequest(name, indexer))
	if err != nil {
		return nil, err
	}
	return memql.EmbedAsIndexer(provider, indexer), nil
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
