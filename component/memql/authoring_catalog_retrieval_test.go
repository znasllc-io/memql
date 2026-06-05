package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// fakeSimilarTo simulates integration.similarity.similarTo: it records the
// args it was called with and returns a pre-canned, already similarity-ranked
// node slice (most-similar first), each carrying _similarity on the payload --
// exactly the shape the real handler produces.
func fakeSimilarTo(t *testing.T, ranked []CatalogNearMatch, capturedArgs *map[string]any) builtinExecutorHandler {
	t.Helper()
	return func(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		if capturedArgs != nil {
			*capturedArgs = args
		}
		nodes := make([]memorynodes.MemoryNode, 0, len(ranked))
		for _, m := range ranked {
			payload := map[string]any{
				"name":            m.Name,
				"kind":            m.Kind,
				"catalogKey":      m.CatalogKey,
				"targetNamespace": m.Namespace,
				"_similarity":     m.Similarity,
			}
			raw, _ := json.Marshal(payload)
			nodes = append(nodes, memorynodes.MemoryNode{
				ID:      "v1:authoring:construct:" + m.Name,
				Concept: memorynodes.ConceptAuthoringConstruct,
				Payload: raw,
			})
		}
		return nodes, nil
	}
}

// TestCatalogNearMatches_RanksAndMaps: the fuzzy tier returns the candidates
// in the handler's similarity order, mapping payload -> CatalogNearMatch.
func TestCatalogNearMatches_RanksAndMaps(t *testing.T) {
	ranked := []CatalogNearMatch{
		{CatalogEntry: CatalogEntry{Name: "isAdmin", Kind: "spec", CatalogKey: "spec:aaa"}, Namespace: "owner:u1", Similarity: 0.94},
		{CatalogEntry: CatalogEntry{Name: "isOwner", Kind: "spec", CatalogKey: "spec:bbb"}, Namespace: "owner:u1", Similarity: 0.71},
	}
	var got map[string]any
	out, err := catalogNearMatches(context.Background(), fakeSimilarTo(t, ranked, &got), "kind:spec intent:admin gate form:_ actor.role==\"admin\"", 5)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 ranked matches, got %d: %+v", len(out), out)
	}
	if out[0].Name != "isAdmin" || out[0].Similarity != 0.94 {
		t.Errorf("top match should be isAdmin@0.94, got %+v", out[0])
	}
	if out[1].Name != "isOwner" || out[1].Similarity != 0.71 {
		t.Errorf("second match should be isOwner@0.71, got %+v", out[1])
	}
	if out[0].Namespace != "owner:u1" || out[0].Kind != "spec" || out[0].CatalogKey != "spec:aaa" {
		t.Errorf("match fields not mapped from payload: %+v", out[0])
	}

	// The handler must be scoped to the authoring-construct concept and pass
	// the embed text + limit through.
	if got["concept"] != memorynodes.ConceptAuthoringConstruct {
		t.Errorf("expected concept=%q, got %v", memorynodes.ConceptAuthoringConstruct, got["concept"])
	}
	if got["text"] == "" || got["text"] == nil {
		t.Errorf("expected the MatchText to be passed as text, got %v", got["text"])
	}
	if got["limit"] != 5 {
		t.Errorf("expected limit=5 forwarded, got %v", got["limit"])
	}
}

// TestCatalogNearMatches_NoLimit: omitting a limit does not forward a limit
// arg (lets similarTo apply its own default).
func TestCatalogNearMatches_NoLimit(t *testing.T) {
	var got map[string]any
	if _, err := catalogNearMatches(context.Background(), fakeSimilarTo(t, nil, &got), "some need", 0); err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, present := got["limit"]; present {
		t.Errorf("limit<=0 should not forward a limit arg, got %v", got["limit"])
	}
}

// TestCatalogNearMatches_EmptyMatchText: an empty need is a programming error,
// not a query.
func TestCatalogNearMatches_EmptyMatchText(t *testing.T) {
	if _, err := catalogNearMatches(context.Background(), fakeSimilarTo(t, nil, nil), "   ", 5); err == nil {
		t.Errorf("expected an error for empty matchText")
	}
}

// TestCatalogNearMatches_HandlerError: a similarTo failure surfaces (wrapped)
// rather than being swallowed -- the planner needs to know retrieval failed.
func TestCatalogNearMatches_HandlerError(t *testing.T) {
	boom := func(_ context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		return nil, fmt.Errorf("embed provider unavailable")
	}
	if _, err := catalogNearMatches(context.Background(), boom, "need", 5); err == nil {
		t.Errorf("expected the handler error to surface")
	}
}

// TestCatalogNearMatches_SkipsUndecodableRows: a row whose payload doesn't
// decode is skipped, not fatal -- the rest of the ranked set still returns.
func TestCatalogNearMatches_SkipsUndecodableRows(t *testing.T) {
	handler := func(_ context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
		good, _ := json.Marshal(map[string]any{"name": "ok", "kind": "spec", "_similarity": 0.5})
		return []memorynodes.MemoryNode{
			{ID: "bad", Concept: memorynodes.ConceptAuthoringConstruct, Payload: json.RawMessage(`{not json`)},
			{ID: "good", Concept: memorynodes.ConceptAuthoringConstruct, Payload: good, CreatedAt: time.Now()},
		}, nil
	}
	out, err := catalogNearMatches(context.Background(), handler, "need", 5)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 1 || out[0].Name != "ok" {
		t.Errorf("expected the undecodable row skipped and 'ok' returned, got %+v", out)
	}
}
