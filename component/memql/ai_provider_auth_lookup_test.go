package memql

import (
	"context"
	"strings"
	"testing"
)

// ai_provider_auth_lookup_test.go -- memql#4338.
//
// # The drift
//
// `authConceptLookupNames` elides `MEMQL_SI_`. Every live provider in
// dsl/providers/providers.memql asks for `MEMQL_AI_...`, and `MEMQL_SI_`
// survives only as a deprecated alias -- so the elision fired for NO provider
// in the tree and the seal-floor fallback it exists to provide never happened.
//
// Three places documented the behaviour that was missing, which is what makes
// this a code bug rather than a documentation one:
//
//   - the function's own doc comment, which said the prefix was `MEMQL_SI_`
//     and then gave `MEMQL_AI_OPENAI_API_KEY` as the example of it;
//   - the comment at the OS-env fallback, "`MEMQL_OPENAI_API_KEY` in env wins
//     for `MEMQL_AI_OPENAI_API_KEY`-referencing providers";
//   - docs/public/operate/env-vars.md, which spells the mapping out as
//     `authConceptLookupNames("MEMQL_AI_OPENAI_API_KEY")
//     -> ["MEMQL_AI_OPENAI_API_KEY", "MEMQL_OPENAI_API_KEY"]`.
//
// # Why both prefixes
//
// `MEMQL_SI_` is retained, not replaced. A product DSL bundle mounted at
// MEMQL_DSL_PATH is loaded from disk at boot and may still declare providers
// on the old prefix; dropping it would break exactly the installs the alias
// table exists to carry.

func TestAuthConceptLookupNamesElidesTheProviderPrefix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			// THE BUG. Every provider in the tree asks in this shape.
			name: "MEMQL_AI_ falls back to the seal-floor name",
			in:   "MEMQL_AI_ANTHROPIC_API_KEY",
			want: []string{"MEMQL_AI_ANTHROPIC_API_KEY", "MEMQL_ANTHROPIC_API_KEY"},
		},
		{
			name: "MEMQL_AI_ openai, the example the docs use",
			in:   "MEMQL_AI_OPENAI_API_KEY",
			want: []string{"MEMQL_AI_OPENAI_API_KEY", "MEMQL_OPENAI_API_KEY"},
		},
		{
			// Retained: a product bundle may still declare the old prefix.
			// Retained, and it gains the documented seal-floor name. The
			// bare form stays LAST: it is what this returned before
			// memql#4338, kept so a product bundle on the old prefix does
			// not regress, but the documented name wins when both are seeded.
			name: "MEMQL_SI_ elides to the seal-floor name, bare form kept last",
			in:   "MEMQL_SI_ACME_API_KEY",
			want: []string{"MEMQL_SI_ACME_API_KEY", "MEMQL_ACME_API_KEY", "ACME_API_KEY"},
		},
		{
			// A non-AI placeholder must NOT grow a synthesized fallback --
			// that would widen the search to a name nobody declared.
			name: "an unprefixed name is looked up verbatim",
			in:   "MEMQL_ANTHROPIC_API_KEY",
			want: []string{"MEMQL_ANTHROPIC_API_KEY"},
		},
		{
			name: "an unrelated name is looked up verbatim",
			in:   "SOME_OTHER_KEY",
			want: []string{"SOME_OTHER_KEY"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := authConceptLookupNames(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("authConceptLookupNames(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("authConceptLookupNames(%q) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

// The end-to-end claim the issue actually makes: a value seeded under the
// documented SEAL-FLOOR name resolves for a provider that asks for the
// MEMQL_AI_ name. Driven through authConceptResolver, which is what the
// provider registration path calls.
func TestSealFloorSecretResolvesForAnMemqlAIPlaceholder(t *testing.T) {
	const (
		placeholder = "MEMQL_AI_ANTHROPIC_API_KEY" // what the provider asks for
		sealFloor   = "MEMQL_ANTHROPIC_API_KEY"    // what the operator seeded
		value       = "sk-ant-seeded-under-the-documented-name"
	)

	var asked []string
	prevSecret, prevVar := systemSecretResolver, systemVariableResolver
	t.Cleanup(func() { systemSecretResolver, systemVariableResolver = prevSecret, prevVar })

	systemSecretResolver = func(_ context.Context, name string) (string, error) {
		asked = append(asked, name)
		if name == sealFloor {
			return value, nil
		}
		return "", nil
	}
	systemVariableResolver = nil

	got, ok := authConceptResolver(placeholder)
	if !ok {
		t.Fatalf("a globalSecret seeded as %q did not resolve for a provider asking %q.\n"+
			"Names tried: %v\n"+
			"This is the bridge docs/public/operate/env-vars.md documents, and the reason the "+
			"Anthropic base provider could not find a key seeded under the name the docs name "+
			"(memql#4338).", sealFloor, placeholder, asked)
	}
	if got != value {
		t.Fatalf("resolved %q, want %q", got, value)
	}

	// The EXACT name is tried first: a value seeded under the placeholder's
	// own name must win over the seal-floor fallback, or seeding the precise
	// name an operator was asked for would be the losing option.
	if len(asked) == 0 || asked[0] != placeholder {
		t.Errorf("first name tried was %v, want %q first -- the exact match must take priority",
			asked, placeholder)
	}
}

// The error an operator actually reads when nothing resolves must name the
// names that were tried, so "seed it under X" is answerable from the message
// alone.
func TestUnresolvedAuthErrorNamesTheCandidates(t *testing.T) {
	names := authConceptLookupNames("MEMQL_AI_ANTHROPIC_API_KEY")
	joined := strings.Join(names, ", ")
	for _, want := range []string{"MEMQL_AI_ANTHROPIC_API_KEY", "MEMQL_ANTHROPIC_API_KEY"} {
		if !strings.Contains(joined, want) {
			t.Errorf("candidate list %q does not name %q, so the unresolved-auth error cannot "+
				"tell an operator which names were searched", joined, want)
		}
	}
}
