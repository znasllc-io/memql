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
//  3. nobody: the call is its caller's, as every other fleet call is. This
//     includes a binding that names no indexer yet (see below).
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
// # The indexer must still be an owner
//
// Only a cluster owner -- stored role `owner`, active -- is ever an indexer,
// because the indexer's machines see every person's memory text. A binding
// row outlives the person: if its indexer is demoted or deactivated their
// machines must stop embedding the cluster's text at once, not at the next
// boot. TRIGGER: every resolution that names an indexer, whether from the
// context or from the binding. EVIDENCE: the engine's own read of active
// users with the stored role `owner` (IndexerIsActiveOwner, cached 60 s per
// replica, so a demotion reaches every replica within a minute and the hot
// embedding path costs no read). CHOSEN PATH: the embedding is REFUSED with a
// plain reason (ErrEmbeddingIndexerNotOwner); writers already log "until
// backfilled" and skip, and a later backfill catches the item up once an owner
// chooses the model again (Cluster > Settings > Memory search). ALTERNATIVE
// rejected: falling back to the caller, which would quietly send the text to
// the machines of whoever wrote it and mix two machines' vectors in one
// index. STOP CONDITION: an owner chooses the model, which writes a new
// indexer. An EMPTY indexer is not refused: it is a binding made before
// indexers existed, resolved as its caller until adoptEmbedderIndexer names an
// owner at boot. With no check installed (SetEmbeddingIndexerOwnerCheck, set
// by the app) nothing is re-checked, which only tests without an engine see.
//
// Regression tests: embedder_indexer_test.go, app/embedding_indexer_test.go,
// integrations/agent/worker/memory_search_hop_test.go.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

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
// on ctx is resolved as, or "" when it is its caller's own. A non-empty indexer
// that is no longer an active owner is an error (ErrEmbeddingIndexerNotOwner).
//
// The binding is read only for a fleet reference the context names no indexer
// for, so a caller that already holds the binding costs no second read. A read
// that fails borrows nobody's machines: the call is its caller's, which is
// what it was before indexers existed -- and is logged, because it also means
// the cluster's one index may be getting vectors from a writer's machine.
func EmbeddingIndexerForCall(ctx context.Context, providerRef string) (string, error) {
	ref := strings.TrimSpace(providerRef)
	if _, fleet := IsFleetReference(ref); !fleet {
		return "", nil
	}
	indexer, ok := EmbeddingIndexerFromContext(ctx)
	if !ok {
		b, err := CurrentEmbedderBinding(ctx)
		if err != nil {
			if !errors.Is(err, ErrNoEmbedderBound) {
				slog.Warn("memory search: reading the embedder binding failed; embedding as the caller", "provider", ref, "error", err)
			}
			return "", nil
		}
		if strings.TrimSpace(b.ProviderRef) != ref {
			return "", nil
		}
		indexer, _ = IndexerFor(b)
	}
	if indexer == "" {
		return "", nil
	}
	if check := indexerOwnerCheck(); check != nil && !check(ctx, indexer) {
		return "", ErrEmbeddingIndexerNotOwner
	}
	return indexer, nil
}

// ErrEmbeddingIndexerNotOwner refuses a memory-search embedding whose indexer
// is no longer an active cluster owner.
var ErrEmbeddingIndexerNotOwner error = indexerNotOwnerError{}

// indexerNotOwnerError IS-A ErrFleetUnavailable: an indexer who is not an owner
// is "nobody can serve this embedding now", the WAIT every indexing caller
// already handles (the release indexer defers rather than counting a failure;
// writers log "until backfilled"). One sentinel those checks already accept,
// instead of a second classification at each caller.
type indexerNotOwnerError struct{}

func (indexerNotOwnerError) Error() string {
	return "memory search's indexer is no longer an active owner; an owner can choose the model again in Cluster > Settings > Memory search"
}

func (indexerNotOwnerError) Is(target error) bool { return target == ErrFleetUnavailable }

var embeddingIndexerOwnerCheck struct {
	sync.RWMutex
	check func(context.Context, string) bool
}

// SetEmbeddingIndexerOwnerCheck installs the re-check of an indexer's role
// (the app installs Engine.IndexerIsActiveOwner on every replica). Nil removes it.
func SetEmbeddingIndexerOwnerCheck(check func(context.Context, string) bool) {
	embeddingIndexerOwnerCheck.Lock()
	defer embeddingIndexerOwnerCheck.Unlock()
	embeddingIndexerOwnerCheck.check = check
}

func indexerOwnerCheck() func(context.Context, string) bool {
	embeddingIndexerOwnerCheck.RLock()
	defer embeddingIndexerOwnerCheck.RUnlock()
	return embeddingIndexerOwnerCheck.check
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
