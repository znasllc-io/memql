package memql

// embedder_indexer.go -- memory search embeds AS ITS INDEXER, whoever asks
// (design record docs/superpowers/specs/2026-11-23-memory-search-model-switch-design.md,
// section 1, from the owner's decision 7).
//
// # Why
//
// Memory search is ONE index for the whole cluster, and the cluster owner
// provides it. Before this file an embedding resolved against the ACTING
// person's machines, like every other fleet call: an item was embedded on a
// machine of whoever wrote it, and an item written by somebody with no machine
// offering the bound model was simply never indexed. A search query went the
// same way, so a person with no machine could not search at all.
//
// The binding row now records its INDEXER (`indexedBy`): the owner whose
// machines -- their own and the ones lent to them -- embed everything for
// memory search. For a vendor model there is no indexer; the cluster's account
// pays, as it already does for embeddings (component/router accounts.go,
// clusterAccountGated).
//
// # The rule (EmbeddingIndexerForCall)
//
// For a `fleet:` reference, an embedding call is resolved as, in order:
//
//  1. the indexer the CONTEXT names (ContextWithEmbeddingIndexer). The index
//     job sets it for the model it is filling, which during a switch is not
//     the active one yet, and a caller already holding the binding sets it so
//     the binding is not read a second time;
//  2. the ACTIVE binding's indexer, when the name is the active binding's own
//     providerRef;
//  3. nobody: the call is its caller's, as every other fleet call is.
//
// A vendor reference is never changed. The answer reaches the router as the
// request's UserId (app/ai_call_sites.go, embeddingResolveRequest), which the
// engine's attribution keeps (ai_attribution.go), so the decision names the
// indexer.
//
// # Only the embedding step changes hands
//
// Reading and writing the item keeps its usual authority: what changes is
// whose machines turn its text into numbers. EmbedAsIndexer is the half of
// that the router cannot do: the assertion the call carries across a replica
// hop. Its comment has the why.
//
// # Known limitation
//
// The rule follows the binding row. It does not re-check on every call that
// the indexer is still an active cluster owner: the design pauses indexing
// when they stop being one and lets an owner take over by choosing again, and
// that check belongs where indexing is driven and the choice is made, not in a
// second read on every embedding.
//
// Regression tests: embedder_indexer_test.go, app/embedding_indexer_test.go,
// integrations/agent/worker/memory_search_hop_test.go.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// retiredEmbedderBindingPrefix marks a binding row that records a model no
// longer active, whose index is kept for a while (recordRetiredEmbedder).
const retiredEmbedderBindingPrefix = "retired-"

// RetiredEmbedderBindingID is the row id that records a retired model's index:
// "retired-" and the 16 hex characters of its vector table's name
// (EmbeddingVectorTable), so the row and the table it keeps cannot name two
// different models. The width is not part of the id because a model produces
// one width; the row carries it. Empty for a reference or width that has no
// table.
func RetiredEmbedderBindingID(providerRef string, dims int) string {
	table, err := EmbeddingVectorTable(providerRef, dims)
	if err != nil {
		return ""
	}
	return retiredEmbedderBindingPrefix + table[strings.LastIndexByte(table, '_')+1:]
}

// IndexerFor answers whose machines embed for a binding: its indexer, and
// whether it is a fleet model at all. A vendor binding has no indexer whatever
// its row says, because a vendor has no machines.
func IndexerFor(b EmbedderBinding) (userId string, fleet bool) {
	if _, isFleet := IsFleetReference(b.ProviderRef); !isFleet {
		return "", false
	}
	return strings.TrimSpace(b.IndexedBy), true
}

type embeddingIndexerKey struct{}

// ContextWithEmbeddingIndexer names the indexer an embedding on ctx is made
// as. Engine code calls it with an id read off a row an owner's decision wrote
// -- the binding's indexedBy, or the index job's own run -- and never with an
// id a caller supplied: nothing in the DSL or on the wire reaches it.
//
// An empty id returns ctx unchanged, because an empty stamp would shadow an
// outer one with "nobody".
func ContextWithEmbeddingIndexer(ctx context.Context, userId string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	userId = strings.TrimSpace(userId)
	if userId == "" {
		return ctx
	}
	return context.WithValue(ctx, embeddingIndexerKey{}, userId)
}

// EmbeddingIndexerFromContext returns the indexer ctx names, if any.
func EmbeddingIndexerFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	userId, ok := ctx.Value(embeddingIndexerKey{}).(string)
	return userId, ok && userId != ""
}

// EmbeddingIndexerForCall is the rule above: who an embedding of providerRef
// on ctx is resolved as, or "" when it is its caller's own.
//
// The binding is read only for a fleet reference the context names no indexer
// for, so a caller that already holds the binding costs no second read. A read
// that fails borrows nobody's machines: the call is its caller's, which is
// what it was before indexers existed.
func EmbeddingIndexerForCall(ctx context.Context, providerRef string) string {
	ref := strings.TrimSpace(providerRef)
	if _, fleet := IsFleetReference(ref); !fleet {
		return ""
	}
	if userId, ok := EmbeddingIndexerFromContext(ctx); ok {
		return userId
	}
	b, err := CurrentEmbedderBinding(ctx)
	if err != nil || strings.TrimSpace(b.ProviderRef) != ref {
		return ""
	}
	userId, _ := IndexerFor(b)
	return userId
}

// EmbedAsIndexer hands back an embedding provider resolved for the indexer
// whose every call carries the INDEXER'S ASSERTION across a replica hop.
//
// TRIGGER. A fleet call that has to reach a machine whose stream another
// replica holds is forwarded, and the receiving replica checks the machine
// against the subject of the assertion the call carries, refusing an envelope
// whose owner is somebody else (integrations/agent/worker model_forward.go,
// `owner_mismatch`). The bff embeds memory and holds no stream, so every
// memory-search embedding it makes is forwarded. Resolved for the indexer and
// carrying the writer's own assertion, it is refused before start: a writer's
// item would be indexed only when the stream happened to be local.
//
// CHOSEN PATH. For each call, the indexer's writer-scoped assertion
// (auth.ForwardedAuthorityForUser) is bound as the call's forwarded authority
// and nothing else: the form the model pull runner, the pipeline owner and
// the deployer fleet already use for a hop to a person's machine. The actor
// the call runs as stays its caller, and the caller's own context is not
// changed for anything after the call.
//
// ALTERNATIVES. Binding the indexer's whole context (auth.BindForwardedContext,
// as deployer_fleet.go does) would make the call run AS the indexer: a work
// run's provider admission requires the actor to be the run's owner
// (integrations/work provider_admission.go), so every embedding inside
// somebody else's run would be refused, and the call would stop saying who
// asked. Binding nothing leaves the refusal above.
//
// INVARIANT. The subject comes off the binding row or the index job's run,
// never a caller's argument (ContextWithEmbeddingIndexer); it reaches only the
// embedding call of a provider the router resolved for that subject; and a
// test persona never borrows anybody, the same rule the fleet catalog keeps
// (component/worker/fleetcatalog store.go).
//
// A nil provider or an empty indexer is handed back unchanged: there is nobody
// to act as.
func EmbedAsIndexer(p EmbeddingAIProvider, indexer string) EmbeddingAIProvider {
	indexer = strings.TrimSpace(indexer)
	if p == nil || indexer == "" {
		return p
	}
	return indexerEmbedder{inner: p, indexer: indexer}
}

type indexerEmbedder struct {
	inner   EmbeddingAIProvider
	indexer string
}

func (p indexerEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	ctx, err := contextForIndexerHop(ctx, p.indexer)
	if err != nil {
		return nil, err
	}
	return p.inner.Embed(ctx, text)
}

func (p indexerEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	ctx, err := contextForIndexerHop(ctx, p.indexer)
	if err != nil {
		return nil, err
	}
	return p.inner.EmbedBatch(ctx, texts)
}

func (p indexerEmbedder) Dimensions() int { return p.inner.Dimensions() }

// contextForIndexerHop is ctx carrying the indexer's assertion for one call.
// It is verified here, before anything is sent, so a refusal names this seam
// rather than arriving from the far replica as a generic one.
func contextForIndexerHop(ctx context.Context, indexer string) (context.Context, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if auth.IsRoleTestContext(ctx) {
		return nil, fmt.Errorf("memory search: a test persona's call cannot borrow the indexer's machines")
	}
	now := time.Now()
	authority, err := auth.ForwardedAuthorityForUser(&auth.AccessContext{UserId: indexer, Role: auth.RoleWriter}, "", "", time.Time{}, now)
	if err != nil {
		return nil, fmt.Errorf("memory search: asserting the indexer's authority for the machine hop: %w", err)
	}
	if _, err := auth.VerifyForwardedAuthority(authority, now); err != nil {
		return nil, fmt.Errorf("memory search: asserting the indexer's authority for the machine hop: %w", err)
	}
	return auth.ContextWithForwardedAuthority(ctx, authority), nil
}
