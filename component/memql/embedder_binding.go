package memql

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// The embedder is a CLUSTER BINDING, and the vector width belongs to the
// provider (epic memql#5137, D6).
//
// WHAT WAS WRONG. The embedding model was the string literal "embedding3Small"
// in five files, and `node_vectors` was declared `vector(1536)` -- that model's
// width, written into the schema. Together those two facts meant the cluster
// could only ever embed with one paid OpenAI model: changing it required
// editing five files AND a migration, and doing either without the other
// produced a table full of vectors of the wrong width, which is not an error
// anywhere. It is a search space that quietly returns wrong neighbours.
//
// Runtime activation probes the chosen model and records one active binding.
// Vectors are separated by provider identity AND width. Every replica reads the
// durable binding; switching an existing corpus is refused until its reindex
// workflow exists. Legacy vectors remain stored, but are not mixed into a new
// model space whose provenance cannot be established.
//
// WHY A LITERAL ID. The same reason v1:cluster:database is at `primary`
// (memql#4766): a re-write is then a new VERSION of one logical row rather than
// a second row, and "which binding is active" has exactly one answer to read.

// EmbedderBindingConceptID is the concept the active binding lives on.
const EmbedderBindingConceptID = "v1:platform:embedderBinding"

// ActiveEmbedderBindingID is the literal row id of the active binding.
const ActiveEmbedderBindingID = "active"

// EmbedderBinding is the cluster's active embedding configuration.
type EmbedderBinding struct {
	// ProviderRef is a provider record name (embedding3Small) or a fleet
	// reference (fleet:qwen3-embedding:0.6b). It is resolved through the same
	// registry path every other provider name takes, so a fleet embedder is
	// resolved against the ACTING USER's machines like any other fleet call.
	ProviderRef string
	// Dimensions is the vector width this provider produces, and therefore
	// which model-specific vector table its vectors live in. Never zero on a
	// valid binding: a width of zero cannot have a table and cannot have an
	// index, and pgvector refuses both rather than accepting them silently.
	Dimensions int
	// ActivatedAt is when this binding became active, RFC3339.
	ActivatedAt string
	// ReembedRunId names the work run that filled this binding's table, when
	// the binding replaced another. Empty for the first binding a cluster ever
	// activates, which has nothing to re-embed FROM.
	ReembedRunId string
	// PreviousRef is the ProviderRef this binding replaced.
	//
	// KEPT BECAUSE A VECTOR CORPUS IS NOT REVERSIBLE FROM ITS VECTORS. Nothing
	// in a table of floats says which model produced them, so without this the
	// only record of what a search space used to mean is gone the moment it is
	// replaced.
	PreviousRef string
}

// Valid reports whether the binding can actually be used. A binding with no
// provider or no width is a row somebody started writing, and using it would
// create node_vectors_0.
func (b EmbedderBinding) Valid() bool {
	return strings.TrimSpace(b.ProviderRef) != "" && b.Dimensions > 0
}

// VectorTableFor names the table holding vectors of a given width.
//
// ONE TABLE PER WIDTH rather than one table with an untyped column, because a
// pgvector index needs fixed dimensions: an untyped column can be written and
// cannot be searched quickly, which is the failure that looks like the feature
// working until the corpus grows.
func VectorTableFor(dims int) string {
	return fmt.Sprintf("node_vectors_%d", dims)
}

// embedderBindingCache holds the last binding read, so the hot embedding path
// does not re-read a row that changes at most a handful of times in a cluster's
// life. It is invalidated by the same write that activates a new binding.
type embedderBindingCache struct {
	mu     sync.RWMutex
	loaded bool
	value  EmbedderBinding
}

var activeBinding embedderBindingCache

var bindingReader struct {
	sync.RWMutex
	read func(context.Context) (EmbedderBinding, error)
}

// The application installs a durable reader on every replica. No activation
// depends on another process's cache or on receiving an invalidation event.
func SetEmbedderBindingReader(read func(context.Context) (EmbedderBinding, error)) {
	bindingReader.Lock()
	defer bindingReader.Unlock()
	bindingReader.read = read
}

func CurrentEmbedderBinding(ctx context.Context) (EmbedderBinding, error) {
	bindingReader.RLock()
	read := bindingReader.read
	bindingReader.RUnlock()
	if read != nil {
		return read(ctx)
	}
	if binding, ok := ActiveEmbedderBinding(); ok {
		return binding, nil
	}
	return EmbedderBinding{}, ErrNoEmbedderBound
}

// SetActiveEmbedderBinding supplies an in-process fixture binding. Production
// installs a durable reader through SetEmbedderBindingReader on every replica.
func SetActiveEmbedderBinding(b EmbedderBinding) {
	activeBinding.mu.Lock()
	defer activeBinding.mu.Unlock()
	activeBinding.value = b
	activeBinding.loaded = true
}

// ClearActiveEmbedderBinding forgets the cached binding. Used by tests and by
// the reload path; a cleared cache re-reads rather than serving a stale width.
func ClearActiveEmbedderBinding() {
	activeBinding.mu.Lock()
	defer activeBinding.mu.Unlock()
	activeBinding.value = EmbedderBinding{}
	activeBinding.loaded = false
}

// ActiveEmbedderBinding returns the cluster's active binding.
//
// THE SECOND RETURN IS "IS THERE ONE", NOT "DID IT WORK", and the difference
// matters at every call site. A cluster with no binding is the ordinary state
// of a fresh install that has not chosen an embedder yet -- it is not an error,
// and it must not be reported as one. What it IS is a cluster where nothing can
// be embedded, so every caller degrades honestly rather than falling back to a
// model nobody chose.
func ActiveEmbedderBinding() (EmbedderBinding, bool) {
	activeBinding.mu.RLock()
	defer activeBinding.mu.RUnlock()
	if !activeBinding.loaded || !activeBinding.value.Valid() {
		return EmbedderBinding{}, false
	}
	return activeBinding.value, true
}

// ErrNoEmbedderBound is what an embedding call gets when the cluster has no
// active binding.
//
// IT NAMES THE FIX, because the alternative reads as a bug. "embedding provider
// unavailable" sends an operator looking at credentials; what is actually true
// is that nobody has chosen an embedder, and the place to choose one is a
// screen they have probably not opened.
var ErrNoEmbedderBound = fmt.Errorf(
	"no embedder is bound: this cluster has no active %s row, so nothing can be embedded. "+
		"Bind one from Fleet -> Models, or pull a catalog embeddings model onto a machine "+
		"(qwen3-embedding:0.6b is the 16 GB default). There is deliberately no fallback: an "+
		"embedder chosen for you would write vectors into a search space you did not pick",
	EmbedderBindingConceptID,
)

// ErrEmbedderWidthUnknown is what BINDING a model the catalog does not carry
// returns.
//
// IT IS A REAL STATE, NOT A HYPOTHETICAL, and the cockpit session is the reason
// this error exists separately from ErrNoEmbedderBound. A cockpit advertises
// `embeddings=1` for any model whose runtime reports the capability, catalog row
// or not -- so a machine can legitimately OFFER an embedding model that cannot
// be BOUND as the cluster's embedder. An operator running `memql worker models`
// sees it listed and offered, and a bare "cannot be bound" reads as a bug in
// their machine.
//
// So the message names the model and says the missing thing is a CATALOG ROW,
// not a capability. The model still serves embedding calls that name it
// explicitly; what it cannot be is the cluster-wide binding, because the binding
// creates `node_vectors_<dims>` before the first vector exists and a table at a
// guessed width is a search space that silently returns wrong neighbours.
func ErrEmbedderWidthUnknown(modelRef string) error {
	return fmt.Errorf(
		"cannot bind %q as the cluster embedder: the catalog does not record its vector width. "+
			"This is not a problem with the machine offering it -- the model is real and serves "+
			"embedding calls that name it. A BINDING needs the width before the first vector "+
			"exists, because it creates the vector table, and a table at a guessed width returns "+
			"wrong neighbours without ever erroring. Add a %s row for %q with its `dimensions`, "+
			"or bind a curated entry (qwen3-embedding:0.6b is 1024, nomic-embed-text is 768)",
		modelRef, "v1:models:modelProfile", modelRef,
	)
}

// BindingFor builds a binding for a provider reference, resolving the width from
// the catalog for a fleet model.
//
// A NON-FLEET reference carries its width on the provider record, which the
// caller supplies; a `fleet:` one does not, because the machine serving it does
// not know it either. That asymmetry is why this function exists rather than the
// caller filling the struct: it is the one place the two ways of learning a
// width meet, and putting them anywhere else would let one of them be forgotten.
func BindingFor(providerRef string, declaredDimensions int) (EmbedderBinding, error) {
	ref := strings.TrimSpace(providerRef)
	if ref == "" {
		return EmbedderBinding{}, ErrNoEmbedderBound
	}
	if declaredDimensions > 0 {
		return EmbedderBinding{ProviderRef: ref, Dimensions: declaredDimensions}, nil
	}
	modelID := strings.TrimPrefix(ref, FleetReferencePrefix)
	if dims, ok := catalogDimensionsFor(modelID); ok {
		return EmbedderBinding{ProviderRef: ref, Dimensions: dims}, nil
	}
	return EmbedderBinding{}, ErrEmbedderWidthUnknown(ref)
}

// ResolveEmbedderProvider returns the provider name the active binding names.
//
// This is the ONE narrowing from "the cluster's embedder" to "a provider name",
// and every embedding site goes through it -- integrations/embedding, knowledge,
// similarity, harnessRecall and the semantic cache. Before this epic each of
// those carried its own `defaultProvider = "embedding3Small"` constant, which
// is five copies of one decision that could drift, and did not drift only
// because nobody had ever changed it.
func ResolveEmbedderProvider(ctx context.Context) (string, error) {
	b, err := CurrentEmbedderBinding(ctx)
	if err != nil {
		return "", err
	}
	return b.ProviderRef, nil
}

// EmbedderDimensions returns the active binding's vector width, or 0 and false
// when nothing is bound. Callers that need a table name use VectorTableFor on
// the result rather than assuming 1536.
func EmbedderDimensions() (int, bool) {
	b, ok := ActiveEmbedderBinding()
	if !ok {
		return 0, false
	}
	return b.Dimensions, true
}

// catalogWidths holds the vector width the CATALOG records per model id, keyed
// by the runtime's own model id exactly as a machine advertises it.
//
// IT IS A CACHE OF SEEDED DSL DATA, not a second source of truth. The rows live
// in `v1:models:modelProfile` and are re-materialized on every boot; this map is
// filled from them once the engine has loaded, because `fleetProvider.Dimensions`
// is on the embedding hot path and cannot run a query per call.
//
// A model absent from it reports 0, which callers read as "unknown". That is the
// right answer for an operator's own pull that the catalog has never heard of,
// and it is why a binding to such a model is refused rather than guessed: the
// binding's whole job is to know the width before the first vector exists.
var catalogWidths struct {
	mu     sync.RWMutex
	byID   map[string]int
	loaded bool
}

// SetCatalogWidths publishes the catalog's vector widths. Called once the
// modelProfile rows are readable; safe to call again after a re-seed.
func SetCatalogWidths(widths map[string]int) {
	catalogWidths.mu.Lock()
	defer catalogWidths.mu.Unlock()
	next := make(map[string]int, len(widths))
	for id, dims := range widths {
		if id = strings.TrimSpace(id); id != "" && dims > 0 {
			next[id] = dims
		}
	}
	catalogWidths.byID = next
	catalogWidths.loaded = true
}

// catalogDimensionsFor answers the width the catalog records for one model id.
//
// EXACT EQUALITY, never a prefix or a fuzzy match. The model id is byte-identical
// from the cockpit's label to the catalog row to a policy naming
// `fleet:<modelId>` precisely so this comparison can be a string equality --
// `qwen3-embedding:0.6b` and `qwen3-embedding:0.6b-q8` are different models with
// potentially different widths, and matching them to each other would bind a
// table at the wrong size.
func catalogDimensionsFor(modelID string) (int, bool) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return 0, false
	}
	catalogWidths.mu.RLock()
	defer catalogWidths.mu.RUnlock()
	if !catalogWidths.loaded {
		return 0, false
	}
	dims, ok := catalogWidths.byID[modelID]
	return dims, ok && dims > 0
}
