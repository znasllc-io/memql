package app

// call_site_levels_test.go -- the table that pins what every model call app/
// makes declares (epic memql#5127, design D2, record section 6).
//
// # What it is for
//
// A re-pointed call site is a diff somebody read unless something checks it.
// The whole claim of this epic is that a call declares a LEVEL and derives a
// MODALITY -- and a site that silently declared `fast` where it needed
// `strong`, or `chat` where the caller type-asserts a structured provider,
// would compile, resolve, and be wrong in a way that only shows up as a worse
// answer or a mid-call type failure. Neither reads as a routing bug.
//
// So each site's request is a named function in ai_call_sites.go, and this is
// the table over them. A row here is the contract for one site; changing a
// site's level is then a deliberate two-file edit rather than a one-character
// one.
//
// # Why the table asserts the REQUEST and not the resolution
//
// What the router does with a request is the router's own tests. What this
// package is responsible for is the request: the level it asked for, the
// modality it derived, the needs it asserted, and whether a caller-named
// provider became a PIN rather than a lookup. Those are properties of a pure
// function over values, so this test needs no engine, no registry and no
// database -- which is what makes it a gate that actually runs rather than one
// that skips.

import (
	"testing"

	"github.com/znasllc-io/memql/core/airoute"
)

func TestEveryAppCallSiteCarriesTheExpectedLevelAndModality(t *testing.T) {
	cases := []struct {
		site             string
		req              airoute.ResolveRequest
		level            airoute.Level
		modality         airoute.Modality
		wantStructured   bool
		wantVision       bool
		explicitProvider string
		// userId is a caller the request FIXES rather than leaves to the
		// context: only memory search's indexer, for now.
		userId string
	}{
		{
			// plugins.go's ResolveVisionProvider closure and
			// transport_artifacts.go's Library analyzer, both through
			// (*App).resolveVisionProvider.
			site:       "vision (file and image description)",
			req:        visionResolveRequest(),
			level:      airoute.LevelFast,
			modality:   airoute.ModalityVision,
			wantVision: true,
		},
		{
			// plugins.go's ResolveEmbeddingProvider closure, reached by the
			// embedding, knowledge, similarity and harnessrecall packs.
			site:             "embedding (a pinned model id)",
			req:              embeddingResolveRequest("embedding3Small", ""),
			level:            airoute.LevelEmbeddings,
			modality:         airoute.ModalityEmbedding,
			explicitProvider: "embedding3Small",
		},
		{
			site:     "embedding (no pin)",
			req:      embeddingResolveRequest("", ""),
			level:    airoute.LevelEmbeddings,
			modality: airoute.ModalityEmbedding,
		},
		{
			// The same closure for memory search's own model: resolved as the
			// owner who provides the index, whoever is asking
			// (embedding_indexer_test.go).
			site:             "embedding (memory search, as its indexer)",
			req:              embeddingResolveRequest("fleet:qwen3-embedding:0.6b", "owner-1"),
			level:            airoute.LevelEmbeddings,
			modality:         airoute.ModalityEmbedding,
			explicitProvider: "fleet:qwen3-embedding:0.6b",
			userId:           "owner-1",
		},
		{
			// safety_llm.go's classifier chain.
			site:             "safety command classifier",
			req:              safetyClassifierResolveRequest("chat54Mini"),
			level:            airoute.LevelFast,
			modality:         airoute.ModalityStructured,
			wantStructured:   true,
			explicitProvider: "chat54Mini",
		},
		{
			// integrations_work_compile.go's healer, planner node only.
			site:           "healing patch proposal",
			req:            healingPatchResolveRequest(),
			level:          airoute.LevelFast,
			modality:       airoute.ModalityStructured,
			wantStructured: true,
		},
	}

	for _, c := range cases {
		t.Run(c.site, func(t *testing.T) {
			if c.req.Level != c.level {
				t.Errorf("declares level %q, expected %q", c.req.Level, c.level)
			}
			if c.req.Modality != c.modality {
				t.Errorf("derives modality %q, expected %q", c.req.Modality, c.modality)
			}
			// A level or modality outside the closed sets reaches the rule
			// matcher as a condition nothing can satisfy, and presents as a
			// call that mysteriously took the default rule.
			if !c.req.Level.Valid() {
				t.Errorf("level %q is outside the closed four (%s)", c.req.Level, airoute.LevelNames())
			}
			if !c.req.Modality.Valid() {
				t.Errorf("modality %q is one the router does not derive", c.req.Modality)
			}
			if c.req.Needs.Structured != c.wantStructured {
				t.Errorf("Needs.Structured is %v, expected %v -- the schema is either the point of the call or it is not",
					c.req.Needs.Structured, c.wantStructured)
			}
			if c.req.Needs.Vision != c.wantVision {
				t.Errorf("Needs.Vision is %v, expected %v", c.req.Needs.Vision, c.wantVision)
			}
			if c.req.ExplicitProvider != c.explicitProvider {
				t.Errorf("ExplicitProvider is %q, expected %q -- a caller-named provider is a PIN and must ride this field, never a registry lookup",
					c.req.ExplicitProvider, c.explicitProvider)
			}
			if c.req.UserId != c.userId {
				t.Errorf("UserId is %q, expected %q -- a request fixes its caller only when the call is not the asker's own",
					c.req.UserId, c.userId)
			}
		})
	}
}

// TestTheEmbeddingLevelNeverDegrades is a property rather than a row, because
// it is the one thing about these five requests that a plausible edit would
// get wrong without anything else noticing.
//
// A degraded embedder answers in a DIFFERENT VECTOR SPACE. The result is not a
// worse vector -- it is one that does not belong in the index it is about to
// be written to, and nothing downstream can tell the difference: the write
// succeeds, the search returns neighbours, and the neighbours are noise. Level
// `embeddings` is the only level with no next rung down, which is what makes
// that unrepresentable rather than merely discouraged.
func TestTheEmbeddingLevelNeverDegrades(t *testing.T) {
	req := embeddingResolveRequest("embedding3Small", "")
	if _, canDegrade := req.Level.Degrade(); canDegrade {
		t.Fatalf("the embedding site declares level %q, which degrades; an embedding call that falls back to "+
			"another model writes vectors into an index they do not belong in, undetectably", req.Level)
	}
}
