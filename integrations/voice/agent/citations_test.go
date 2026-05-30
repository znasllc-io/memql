package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCitationResolver_NoOpWhenUngrounded verifies the strict no-op: an empty
// grounding context yields no citations, so an ungrounded realtime reply lands
// byte-identical to an ungrounded text reply.
func TestCitationResolver_NoOpWhenUngrounded(t *testing.T) {
	r := NewCitationResolver(GroundingContext{})
	assert.True(t, r.IsEmpty(), "empty grounding -> no-op resolver")
	assert.Nil(t, r.Resolve("the quarterly revenue grew substantially this period"))
}

// TestCitationResolver_FactWithoutDomainIsNotCitable verifies a grounding fact
// carrying no DomainName is not citable (nothing to attribute to) -> no-op.
func TestCitationResolver_FactWithoutDomainIsNotCitable(t *testing.T) {
	r := NewCitationResolver(GroundingContext{Facts: []GroundingFact{
		{Text: "revenue grew twelve percent year over year"},
	}})
	assert.True(t, r.IsEmpty())
	assert.Nil(t, r.Resolve("revenue grew twelve percent year over year"))
}

// TestCitationResolver_VerbatimMatch verifies a phrase that appears verbatim in
// the transcript produces one {domainId, matchedPhrase} citation.
func TestCitationResolver_VerbatimMatch(t *testing.T) {
	r := NewCitationResolver(GroundingContext{Facts: []GroundingFact{
		{Text: "annual recurring revenue reached forty million", DomainName: "finance"},
	}})
	cites := r.Resolve("As I noted, annual recurring revenue reached forty million last year.")
	require.Len(t, cites, 1)
	assert.Equal(t, "finance", cites[0].GetDomainId())
	assert.Equal(t, "annual recurring revenue reached forty million", cites[0].GetMatchedPhrase())
}

// TestCitationResolver_CasePreservedFromTranscript verifies matching is
// case-insensitive but the emitted matchedPhrase preserves the transcript's
// casing (so the frontend's exact indexOf wrap succeeds).
func TestCitationResolver_CasePreservedFromTranscript(t *testing.T) {
	r := NewCitationResolver(GroundingContext{Facts: []GroundingFact{
		{Text: "the migration plan", DomainName: "ops"}, // lowercase in grounding
	}})
	cites := r.Resolve("We discussed The Migration Plan in detail.")
	require.Len(t, cites, 1)
	assert.Equal(t, "The Migration Plan", cites[0].GetMatchedPhrase(),
		"matched phrase must be the verbatim transcript substring, not the grounding casing")
}

// TestCitationResolver_ShortPhraseGuard verifies a phrase below the minimum
// length is not cited (so a one-word fragment cannot match everything).
func TestCitationResolver_ShortPhraseGuard(t *testing.T) {
	r := NewCitationResolver(GroundingContext{Facts: []GroundingFact{
		{Text: "data", DomainName: "kb"}, // 4 chars < minCitationPhraseLen
	}})
	assert.Nil(t, r.Resolve("we looked at the data carefully"))
}

// TestCitationResolver_Dedupe verifies the same (domain, phrase) hit is emitted
// once even when the grounding repeats the phrase across facts.
func TestCitationResolver_Dedupe(t *testing.T) {
	r := NewCitationResolver(GroundingContext{Facts: []GroundingFact{
		{Text: "customer churn analysis", DomainName: "analytics"},
		{Text: "customer churn analysis", DomainName: "analytics"},
	}})
	cites := r.Resolve("Our customer churn analysis shows improvement.")
	require.Len(t, cites, 1, "duplicate (domain, phrase) collapses to one citation")
}

// TestCitationResolver_NoMatch verifies a transcript that contains none of the
// grounding phrases produces no citations.
func TestCitationResolver_NoMatch(t *testing.T) {
	r := NewCitationResolver(GroundingContext{Facts: []GroundingFact{
		{Text: "supply chain logistics overview", DomainName: "ops"},
	}})
	assert.Nil(t, r.Resolve("Let us talk about something entirely different today."))
}

// TestCitationResolver_MultipleDomains verifies distinct domains each cite.
func TestCitationResolver_MultipleDomains(t *testing.T) {
	r := NewCitationResolver(GroundingContext{Facts: []GroundingFact{
		{Text: "quarterly earnings call", DomainName: "finance"},
		{Text: "product roadmap milestones", DomainName: "product"},
	}})
	cites := r.Resolve("The quarterly earnings call and the product roadmap milestones both matter.")
	require.Len(t, cites, 2)
	domains := map[string]bool{}
	for _, c := range cites {
		domains[c.GetDomainId()] = true
	}
	assert.True(t, domains["finance"])
	assert.True(t, domains["product"])
}
