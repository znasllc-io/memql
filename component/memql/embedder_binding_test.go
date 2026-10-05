package memql

import (
	"context"
	"strings"
	"testing"
)

// The embedder binding (epic memql#5137, D6).
//
// These are PURE tests of the decisions -- the width lookup, the table naming,
// the completeness rule, the refusals. They touch no database on purpose: the
// properties that matter here are all answers to questions about values, and a
// db-gated test would make them skip silently on every machine without one,
// which is the failure mode this repo's CLAUDE.md warns about twice.
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

func TestActivationRefusesAWidthOfZero(t *testing.T) {
	// A width of zero would create `node_vectors_0`: pgvector refuses to index
	// it, nothing could ever search it, and it would sit there as a table
	// somebody has to find later. Refusing at the door is what keeps it from
	// existing.
	err := EnsureVectorTable(context.Background(), nil, 0)
	if err == nil {
		t.Fatal("a width of zero must be refused")
	}
	if !strings.Contains(err.Error(), "width") {
		t.Errorf("the refusal must name the problem; got %q", err)
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
		t.Error("activation re-runs on every boot for the active binding, so the DDL must be idempotent")
	}
}

// TestActiveFlipsOnlyOnAMatchedCount is the property that protects the search
// space, and the direction of the comparison is the point.
func TestActiveFlipsOnlyOnAMatchedCount(t *testing.T) {
	if ReembedComplete(1000, 999) {
		t.Error("a partially filled table must not flip the binding: half a corpus in each geometry is not a degraded search space, it is a meaningless one")
	}
	if !ReembedComplete(1000, 1000) {
		t.Error("a matched count must complete")
	}
	// EQUAL, NOT "AT LEAST". More rows in the new table than the old means
	// something else is writing into it -- a concurrent re-embed, a second
	// activation -- and flipping on that makes those rows authoritative without
	// anybody having decided they should be.
	if ReembedComplete(1000, 1001) {
		t.Error("an over-full new table must not complete: the extra rows came from somewhere nobody decided about")
	}
	// A cluster that has embedded nothing has nothing to carry over, and its
	// FIRST binding should activate immediately rather than waiting for a count
	// that will never move.
	if !ReembedComplete(0, 0) {
		t.Error("a cluster with no vectors must be able to activate its first binding")
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

// TestBindingForRefusesAModelTheCatalogDoesNotKnow is the cockpit session's
// case, and it is a real state rather than a hypothetical: a cockpit advertises
// `embeddings=1` for any model whose runtime reports the capability, catalog row
// or not. So a machine can legitimately OFFER an embedding model that cannot be
// BOUND, and the refusal has to say which of those two things is missing.
func TestBindingForRefusesAModelTheCatalogDoesNotKnow(t *testing.T) {
	SetCatalogWidths(map[string]int{"qwen3-embedding:0.6b": 1024})
	t.Cleanup(func() { SetCatalogWidths(map[string]int{}) })

	if _, err := BindingFor("fleet:some-model-nobody-curated", 0); err == nil {
		t.Fatal("binding a model with no catalog width must be refused rather than guessed")
	} else {
		msg := err.Error()
		if !strings.Contains(msg, "some-model-nobody-curated") {
			t.Errorf("the refusal must name the model; got %q", msg)
		}
		// An operator running `memql worker models` sees the model listed and
		// offered. A bare "cannot be bound" reads as a fault in their machine.
		if !strings.Contains(msg, "not a problem with the machine") {
			t.Errorf("the refusal must say the machine is fine; got %q", msg)
		}
		if !strings.Contains(msg, "modelProfile") {
			t.Errorf("the refusal must name what is missing -- a catalog row; got %q", msg)
		}
	}

	b, err := BindingFor("fleet:qwen3-embedding:0.6b", 0)
	if err != nil {
		t.Fatalf("a curated model must bind: %v", err)
	}
	if b.Dimensions != 1024 {
		t.Errorf("width = %d, want the catalog's 1024", b.Dimensions)
	}
}

func TestADeclaredWidthWinsOverTheCatalog(t *testing.T) {
	// A non-fleet provider carries its width on the provider record, and that
	// is the authority for it -- the catalog describes models a machine might
	// pull, not vendor records.
	SetCatalogWidths(map[string]int{"embedding3Small": 999})
	t.Cleanup(func() { SetCatalogWidths(map[string]int{}) })

	b, err := BindingFor("embedding3Small", 1536)
	if err != nil {
		t.Fatalf("a declared width must bind: %v", err)
	}
	if b.Dimensions != 1536 {
		t.Errorf("width = %d, want the declared 1536", b.Dimensions)
	}
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
	if _, err := BindingFor("fleet:qwen3-embedding:0.6b", 0); err == nil {
		t.Error("binding against an unloaded catalog must refuse rather than bind at zero")
	}
}

// The activation plan (epic memql#5137, D6). Pure decisions, no database.

func TestActivationPlanRefusesAnUnknownWidth(t *testing.T) {
	SetCatalogWidths(map[string]int{})
	t.Cleanup(func() { SetCatalogWidths(map[string]int{}) })

	_, err := PlanActivation(
		EmbedderActivation{ProviderRef: "fleet:unknown-model"},
		EmbedderBinding{}, 0,
	)
	if err == nil {
		t.Fatal("planning against a width nobody knows must refuse rather than bind at zero")
	}
}

func TestActivationOnAnEmptyCorpusNeedsNoReembed(t *testing.T) {
	// The FIRST binding a cluster activates has nothing to carry over. Making
	// it wait for a re-embed with no rows to move would leave it permanently in
	// `plan`, and the cluster could never embed anything.
	plan, err := PlanActivation(
		EmbedderActivation{ProviderRef: "embedding3Small", DeclaredDimensions: 1536},
		EmbedderBinding{}, 0,
	)
	if err != nil {
		t.Fatalf("PlanActivation: %v", err)
	}
	if plan.NeedsReembed {
		t.Error("a cluster with no vectors has nothing to re-embed")
	}
	if plan.SameWidth {
		t.Error("there is no previous binding to share a width with")
	}
	if plan.NoChange {
		t.Error("a first binding is a change")
	}
}

func TestReactivatingTheSameBindingIsNoChange(t *testing.T) {
	// Without this the activation re-embeds an entire corpus into the table it
	// is already in -- for a large Library, hours of somebody's laptop spent
	// producing the vectors that are already there.
	current := EmbedderBinding{ProviderRef: "embedding3Small", Dimensions: 1536}
	plan, err := PlanActivation(
		EmbedderActivation{ProviderRef: "embedding3Small", DeclaredDimensions: 1536},
		current, 5000,
	)
	if err != nil {
		t.Fatalf("PlanActivation: %v", err)
	}
	if !plan.NoChange {
		t.Error("re-activating the binding that is already active must be a no-change")
	}
}

func TestTwoEmbeddersOfTheSameWidthStillNeedAReembed(t *testing.T) {
	// SAME WIDTH IS NOT SAME MEANING, and this is the trap the flag is named to
	// avoid. bge-m3 and qwen3-embedding:0.6b are both 1024 and share nothing
	// else -- vectors from one are noise to the other. SameWidth says only that
	// the two bindings share a table NAME, which is a fact about storage.
	SetCatalogWidths(map[string]int{"bge-m3": 1024, "qwen3-embedding:0.6b": 1024})
	t.Cleanup(func() { SetCatalogWidths(map[string]int{}) })

	current := EmbedderBinding{ProviderRef: "fleet:qwen3-embedding:0.6b", Dimensions: 1024}
	plan, err := PlanActivation(EmbedderActivation{ProviderRef: "fleet:bge-m3"}, current, 5000)
	if err != nil {
		t.Fatalf("PlanActivation: %v", err)
	}
	if !plan.SameWidth {
		t.Error("1024 and 1024 is the same width")
	}
	if plan.NoChange {
		t.Error("a different provider at the same width is still a change")
	}
	if !plan.NeedsReembed {
		t.Fatal("a corpus embedded by another model must be rebuilt even at the same width -- its vectors are noise to the new one")
	}
}

func TestThePlanRemembersWhatItReplaced(t *testing.T) {
	// A vector corpus is not reversible from its vectors, so the row is the
	// only record of what produced them.
	current := EmbedderBinding{ProviderRef: "embedding3Small", Dimensions: 1536}
	plan, err := PlanActivation(
		EmbedderActivation{ProviderRef: "embedding3Large", DeclaredDimensions: 3072},
		current, 10,
	)
	if err != nil {
		t.Fatalf("PlanActivation: %v", err)
	}
	if plan.Binding.PreviousRef != "embedding3Small" {
		t.Errorf("previousRef = %q, want the binding it replaced", plan.Binding.PreviousRef)
	}
}

func TestPrepareRefusesAnIncompleteBinding(t *testing.T) {
	a := &EmbedderActivator{}
	if err := a.Prepare(context.Background(), ActivationPlan{}); err == nil {
		t.Fatal("preparing an incomplete binding must be refused")
	}
}
