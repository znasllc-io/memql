//go:build agent || planner

package worker

// MEMORY SEARCH EMBEDS AS ITS INDEXER, ACROSS THE HOP (design record
// 2026-11-23, section 1).
//
// The bff embeds memory and holds no WorkerService stream, so EVERY fleet
// embedding it makes crosses to the agent holding the machine. The receiving
// agent checks the machine against the subject of the assertion the call
// carries, and refuses an envelope whose owner is somebody else
// (`owner_mismatch`, model_forward.go). So resolving the bound model for the
// indexer is not enough on its own: a writer's embedding, resolved for the
// indexer and carrying the writer's own assertion, is refused before start,
// and would only ever have worked when the stream happened to be local.
//
// In process, for the reason every hop test here gives: a live-cluster gate is
// skipped on every CI lane and developer machine. Only the wire crosses: the
// receiving handler has none of the sending replica's context.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

func TestAMemorySearchEmbeddingCrossesTheHopAsItsIndexer(t *testing.T) {
	const (
		model   = "memory-embedder"
		indexer = "alice" // the owner who chose the model; the machine is hers
		writer  = "bob"   // wrote the item; no machine of his offers the model
	)
	labels := map[string]string{workerservice.ModelLabel(model): (ModelAttributes{Embeddings: true, ContextWindow: 32768}).String()}
	// remoteFleetStore answers alice's machines and nobody else's, on both
	// sides of the hop.
	store := &remoteFleetStore{candidates: []Candidate{{RegistrationId: "laptop", ConnectedNodeId: "agent-with-stream", LastSeenAt: time.Now(),
		Capabilities: []string{workerservice.ModelCapability}, Labels: labels}}}
	registry := workerservice.NewRegistry(slog.Default(), time.Now)
	machine := &workerservice.Worker{RegistrationId: "laptop", OwnerUserId: indexer, Capabilities: []string{workerservice.ModelCapability},
		Labels: labels, Concurrency: map[string]uint32{workerservice.ModelCapability: 1}}
	var served atomic.Int32
	machine.SetModelCallFunc(func(ctx context.Context, req workerservice.ModelCallRequest) (*workerservice.ModelCallHandle, error) {
		if access, ok := auth.AccessFromContext(ctx); !ok || access.UserId != indexer {
			return nil, fmt.Errorf("the machine was reached as somebody other than its owner")
		}
		if req.Kind != workerservice.ModelCallKindEmbedding || len(req.EmbeddingInput) != 1 {
			return nil, fmt.Errorf("unexpected model request")
		}
		served.Add(1)
		handle, _, finish := workerservice.NewModelCallLoopback(req, func(string) {})
		vector := make([]float32, 1024)
		vector[0] = 1
		go finish(workerservice.ModelCallOutcome{FinishReason: workerservice.ModelFinishStop, Embeddings: [][]float32{vector}})
		return handle, nil
	})
	registry.Add(machine)
	link := &plannerModelLink{handler: NewForwardHandler(registry, &remoteFleetStore{candidates: append([]Candidate(nil), store.candidates...)}, slog.Default())}
	link.router = newForwardRouter(link, func() (string, string) { return "bff", "bff" }, slog.Default())
	providers := memqlengine.NewProviderRegistryForTest()
	providers.SetFleetInference(NewRemoteFleetInference(store, link.router, "bff", slog.Default()))

	// The writer's own session on the bff: their assertion, as the stream
	// boundary projects it.
	authority, err := auth.ForwardedAuthorityForUser(&auth.AccessContext{UserId: writer, Role: auth.RoleWriter}, "", "", time.Time{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	access, err := auth.VerifyForwardedAuthority(authority, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	session, cancel := context.WithTimeout(auth.BindForwardedContext(context.Background(), authority.Principal().Claims, access, authority), 5*time.Second)
	defer cancel()
	ctx := memqlengine.ContextWithBackgroundLane(session)

	// Resolved for the indexer, as the plug-in resolver does for the bound
	// model (app/plugins.go).
	entry, ok := providers.EntryForUser(ctx, indexer, "fleet:"+model)
	if !ok || !entry.Available {
		t.Fatalf("the indexer's machine must resolve: %+v", entry)
	}
	client, ok := entry.Client.(memqlengine.EmbeddingAIProvider)
	if !ok {
		t.Fatalf("a fleet entry is not an embedding provider: %T", entry.Client)
	}

	// Carrying the writer's assertion, the receiving agent refuses it.
	if _, err := client.Embed(ctx, "the writer's note, as the writer"); err == nil || !strings.Contains(err.Error(), "owner_mismatch") {
		t.Fatalf("the hop must refuse a call for the indexer's machine on the writer's assertion as owner_mismatch, got %v", err)
	}
	if served.Load() != 0 {
		t.Fatalf("a refused call reached the machine %d time(s)", served.Load())
	}

	// Carrying the indexer's, it is served on her machine.
	vector, err := memqlengine.EmbedAsIndexer(client, indexer).Embed(ctx, "the writer's note, as the indexer")
	if err != nil {
		t.Fatalf("the indexer's embedding was refused across the hop: %v", err)
	}
	if len(vector) != 1024 || served.Load() != 1 || link.target != "agent-with-stream" {
		t.Fatalf("lost the embedding or its receiver: len=%d served=%d target=%q", len(vector), served.Load(), link.target)
	}
	// And the writer's session is the writer's again after the call.
	if a, _ := auth.ForwardedAuthorityFromContext(ctx); a.Subject != writer {
		t.Fatalf("the call changed its caller's assertion to %q", a.Subject)
	}
}
