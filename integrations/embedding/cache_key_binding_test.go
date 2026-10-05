package embedding

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// cache_key_binding_test.go -- the embedding cache key separates bindings that
// differ only in WIDTH (epic memql#5137).
//
// WHY THE PROVIDER NAME IS NOT ENOUGH. `embedding_cache` was keyed by text and
// provider, which was adequate while one embedder was pinned by name in five
// files. With a binding it is not: qwen3-embedding truncates from 4096 all the
// way down to 32, so one provider name covers several geometries. Keyed on the
// name alone, re-binding that provider from 4096 to 1024 serves the old entry
// as though it were the new one.
//
// THE FAILURE IS NOT A STALE ANSWER. It is a vector from another space returned
// as this one's, and a cosine distance against it is a number with no meaning.
// Nothing downstream can tell: the search returns plausible neighbours that are
// simply wrong, which is the same silent corruption the binding exists to
// prevent on the write side.

func TestCacheKeySeparatesBindingsOfDifferentWidth(t *testing.T) {
	t.Cleanup(memql.ClearActiveEmbedderBinding)

	const provider = "qwen3-embedding:0.6b"
	const text = "the same sentence, embedded twice"

	memql.SetActiveEmbedderBinding(memql.EmbedderBinding{ProviderRef: provider, Dimensions: 4096})
	wide := cacheKey(text, provider, activeBindingID(context.Background()))

	memql.SetActiveEmbedderBinding(memql.EmbedderBinding{ProviderRef: provider, Dimensions: 1024})
	narrow := cacheKey(text, provider, activeBindingID(context.Background()))

	if wide == narrow {
		t.Fatalf("one provider at two widths produced one cache key (%s).\n"+
			"The 4096-dimension entry would be served for a 1024-dimension binding -- not a stale\n"+
			"answer but a vector from another geometry, and cosine distance against it is a number\n"+
			"with no meaning. The width must be part of the binding id.", wide)
	}
}

// TestCacheKeyIsStableForOneBinding is the other half, and without it the test
// above passes for a key that changes on every call -- which would separate the
// two widths by never hitting the cache at all.
func TestCacheKeyIsStableForOneBinding(t *testing.T) {
	t.Cleanup(memql.ClearActiveEmbedderBinding)

	memql.SetActiveEmbedderBinding(memql.EmbedderBinding{ProviderRef: "embedding3Small", Dimensions: 1536})
	first := cacheKey("some text", "embedding3Small", activeBindingID(context.Background()))
	second := cacheKey("some text", "embedding3Small", activeBindingID(context.Background()))

	if first != second {
		t.Fatalf("the same text under the same binding produced two keys (%s, %s); nothing would ever hit the cache", first, second)
	}
}

// TestUnboundClusterKeysWithoutABinding pins the caller-named-a-provider path.
// An empty binding id is correct there: the caller chose the provider, so there
// is no cluster binding for the entry to belong to.
func TestUnboundClusterKeysWithoutABinding(t *testing.T) {
	memql.ClearActiveEmbedderBinding()
	if got := activeBindingID(context.Background()); got != "" {
		t.Errorf("an unbound cluster reported binding id %q; it must be empty", got)
	}
}
