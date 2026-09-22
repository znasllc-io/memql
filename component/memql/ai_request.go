package memql

// ai_request.go -- building the router request for the two paths that render a
// PROMPT (epic memql#5127, design D2 and D3).
//
// # Why this is one function and not three
//
// `ai()`, `InvokeAIStructured` and the tool loop each used to resolve a
// provider their own way, and the three disagreed: the prompt path honoured a
// context override, the structured path did not, and the tool loop took a
// context-FREE registry entry, so a fleet model on the caller's own awake
// laptop resolved against the shared catalog and reported itself unavailable.
// Three spellings of one decision is how they came to differ, and nothing said
// so -- each was correct in isolation.
//
// # What a prompt contributes and what it does not
//
// A prompt declares a LEVEL, which is what a rule branches on. It does not
// declare a modality: that is derived from the CALL, because one prompt is
// rendered into a plain chat turn by `ai()` and into a structured turn by
// InvokeAIStructured, and a modality on the prompt would have to be wrong for
// one of them.
//
// `@defaultProvider` survives as an EXPLICIT PIN rather than as a resolution
// step. It rides ExplicitProvider, still wins over every rule, and is still
// refused at load when it names a policy -- the silent downgrade that rule
// prevents is recorded in prompt_default_provider.go.

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/airoute"
)

// requestForPrompt builds the router request for one rendered prompt.
//
// renderedText is the prompt AFTER rendering, because the context floor is a
// question about what will actually be sent. Estimating from the template
// would under-count every prompt whose data is larger than its wording, which
// is most of them.
//
// IT TAKES A CONTEXT BECAUSE ATTRIBUTION IS ON IT (memql#5581). The prompt
// says what the call NEEDS; only the context says who caused it, which
// upstream request it belongs to, which run and step it serves, and what it is
// about. ai_attribution.go is that derivation, and states why it is a context
// read rather than a parameter threaded through the shape evaluator.
func requestForPrompt(ctx context.Context, prompt *PromptTemplate, invocation *AIInvocation, modality airoute.Modality, renderedText string) (airoute.ResolveRequest, error) {
	if prompt == nil {
		return airoute.ResolveRequest{}, fmt.Errorf("no prompt to resolve a request for")
	}
	level, err := airoute.ParseLevel(strings.TrimSpace(prompt.Level))
	if err != nil {
		// A prompt with no level does not reach here in a healthy tree: the
		// loader refuses one at boot. Saying which PROMPT matters anyway,
		// because the one way to get here is a bundle mounted with
		// MEMQL_DSL_ALLOW_SKIPS, and at that point the operator has already
		// been told they are running a tree with load problems.
		return airoute.ResolveRequest{}, fmt.Errorf("prompt %q: %w", prompt.Name, err)
	}

	req := airoute.ResolveRequest{
		Level:      level,
		Modality:   modality,
		PromptName: prompt.Name,
		Needs: airoute.Needs{
			Structured:       modality == airoute.ModalityStructured,
			Tools:            modality == airoute.ModalityTools || modality == airoute.ModalityStreamingTools,
			MinContextTokens: airoute.EstimateMinContextTokens(renderedText, 0),
		},
	}

	// THE PIN, IN PRECEDENCE ORDER, and both halves are pins rather than
	// resolution steps: a context override is one caller saying "this call, this
	// model", and @defaultProvider is an author saying the same about one
	// prompt. Neither consults a rule, and both land on the decision record as
	// an explicit provider so a reader can tell a pinned call from a routed one.
	if invocation != nil && invocation.ProviderOverride != nil {
		if pinned := strings.TrimSpace(*invocation.ProviderOverride); pinned != "" {
			req.ExplicitProvider = pinned
		}
	}
	if req.ExplicitProvider == "" {
		req.ExplicitProvider = strings.TrimSpace(prompt.DefaultProvider)
	}
	return applyCallAttribution(ctx, req), nil
}
