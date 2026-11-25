package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The embedder binding (epic memql#5137, D6).
//
// These are PURE tests of the decisions -- the width lookup, the table naming,
// the refusals. They touch no database on purpose: the properties that matter
// here are all answers to questions about values, and a db-gated test would
// make them skip silently on every machine without one, which is the failure
// mode this repo's CLAUDE.md warns about twice.
//
// What they cannot cover is that the DDL runs, which is what the migration lane
// and a live cluster are for.

func TestVectorTableIsNamedForItsWidth(t *testing.T) {
	// This is the legacy DDL naming primitive. Runtime spaces additionally
	// include provider identity; equal dimensions do not imply equal geometry.
	for dims, want := range map[int]string{
		768:  "node_vectors_768",
		1024: "node_vectors_1024",
		1536: "node_vectors_1536",
		2560: "node_vectors_2560",
		4096: "node_vectors_4096",
	} {
		if got := VectorTableFor(dims); got != want {
			t.Errorf("VectorTableFor(%d) = %q, want %q", dims, got, want)
		}
	}
}

func TestTheDDLDeclaresTheWidthAndIndexesIt(t *testing.T) {
	// An UNTYPED vector column would accept every width and be indexable by
	// none -- a table that writes fine and searches by sequential scan forever,
	// which looks exactly like the feature working until the corpus is big
	// enough for it not to.
	stmts := createVectorTableSQL(768)
	joined := strings.Join(stmts, "\n")
	if !strings.Contains(joined, "vector(768)") {
		t.Error("the column must declare its width; an untyped vector column cannot be indexed")
	}
	if strings.Count(joined, "USING hnsw") != 2 {
		t.Errorf("both vector fields must be indexed, got %d hnsw index(es)", strings.Count(joined, "USING hnsw"))
	}
	if !strings.Contains(joined, "IF NOT EXISTS") {
		t.Error("every store and every tick of the memory index job ensures the table, so the DDL must be idempotent")
	}
}

func TestNoBindingIsNotAnError(t *testing.T) {
	// A fresh install has no binding and that is its ordinary state, not a
	// fault. Every caller degrades honestly instead of reaching for a model
	// nobody chose.
	ClearActiveEmbedderBinding()
	if _, ok := ActiveEmbedderBinding(); ok {
		t.Fatal("a cleared binding must report absent")
	}
	if _, err := ResolveEmbedderProvider(context.Background()); err == nil {
		t.Fatal("resolving with no binding must refuse rather than pick one")
	}
}

func TestAnIncompleteBindingIsNotActive(t *testing.T) {
	// A row somebody started writing -- a provider with no width, or a width
	// with no provider -- must not be served. Using it would create
	// node_vectors_0, or embed with nothing.
	ClearActiveEmbedderBinding()
	t.Cleanup(ClearActiveEmbedderBinding)

	SetActiveEmbedderBinding(EmbedderBinding{ProviderRef: "fleet:x", Dimensions: 0})
	if _, ok := ActiveEmbedderBinding(); ok {
		t.Error("a binding with no width must not be active")
	}
	SetActiveEmbedderBinding(EmbedderBinding{ProviderRef: "", Dimensions: 1024})
	if _, ok := ActiveEmbedderBinding(); ok {
		t.Error("a binding with no provider must not be active")
	}
	SetActiveEmbedderBinding(EmbedderBinding{ProviderRef: "fleet:x", Dimensions: 1024})
	if _, ok := ActiveEmbedderBinding(); !ok {
		t.Error("a complete binding must be active")
	}
}

// With no model bound, an embedding says where one is chosen and by whom:
// Memory search, in the Cluster app's settings, by a cluster owner. Fleet's
// Models page is where a model is pulled onto a machine, not where memory
// search's model is chosen, and a reader who cannot open Memory search learns
// who can.
func TestNoEmbedderBoundSaysWhereAndWhoChooses(t *testing.T) {
	msg := ErrNoEmbedderBound.Error()
	require.Contains(t, msg, "A cluster owner chooses one in Cluster > Settings > Memory search")
	require.NotContains(t, msg, "Fleet")
}

func TestCatalogWidthsMatchExactly(t *testing.T) {
	// The model id is byte-identical from the cockpit's label through the
	// catalog row to a policy naming fleet:<modelId>, precisely so this can be
	// a string equality. A quantised variant is a different model and may have
	// a different width; matching one to the other binds a table at the wrong
	// size, and a mismatched vector column is not an error anywhere -- it is a
	// search space that returns wrong neighbours.
	SetCatalogWidths(map[string]int{"qwen3-embedding:0.6b": 1024})
	t.Cleanup(func() { SetCatalogWidths(map[string]int{}) })

	if _, ok := catalogDimensionsFor("qwen3-embedding:0.6b-q8"); ok {
		t.Error("a quantised variant must not match the base model's width")
	}
	if _, ok := catalogDimensionsFor("qwen3-embedding"); ok {
		t.Error("a prefix must not match")
	}
	if dims, ok := catalogDimensionsFor("qwen3-embedding:0.6b"); !ok || dims != 1024 {
		t.Errorf("the exact id must match: got %d, %v", dims, ok)
	}
}

func TestAnUnloadedCatalogReportsUnknownRatherThanZero(t *testing.T) {
	// Before the profiles are read, every width is UNKNOWN -- and unknown must
	// not read as zero, because zero is a width that would create a table.
	catalogWidths.mu.Lock()
	catalogWidths.byID, catalogWidths.loaded = nil, false
	catalogWidths.mu.Unlock()

	if _, ok := catalogDimensionsFor("qwen3-embedding:0.6b"); ok {
		t.Error("an unloaded catalog must report unknown for every model")
	}
}
