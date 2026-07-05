package agent

// citations.go derives structured {domainId, matchedPhrase} citations from a
// realtime turn's spoken transcript by post-hoc phrase-matching against the
// grounding context that was injected into the session (#456 grounding ->
// #458 citations). It is the Go port of
// the Python voice-agent's realtime_output.py's CitationResolver +
// build_citation_resolver.
//
// Why phrase matching. The cascade carries citations on the respondToUser
// envelope -- cognition knows exactly which domains it grounded on. The
// realtime model speaks directly and never enters that path, so the only
// signal we have is the spoken text and the grounding we injected. The
// resolver scans the transcript for verbatim occurrences of the injected
// grounding phrases and emits one citation per domain hit -- the exact
// {domainId, matchedPhrase} shape handleVoiceAgentRealtimeOutput persists and
// the frontend's splitTextAtCitations renderer (frontend#135) wraps in chips.
//
// Strict no-op when ungrounded. An empty grounding context yields zero
// citations, so an ungrounded realtime reply lands byte-for-byte identical to
// an ungrounded text reply (the handler drops the citations clause entirely
// when empty). Everything here is pure and deterministic -- no stream, no
// provider client -- so it is unit-tested in the default CGO-free lane.

import (
	"sort"
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// minCitationPhraseLen is the shortest grounding phrase worth citing. A phrase
// below this length is skipped so a one-word fragment ("the", "data") cannot
// match everything and litter the reply with spurious chips. Mirrors
// realtime_output.py::CitationResolver.min_phrase_len.
const minCitationPhraseLen = 8

// citationDomain is one knowledge domain's grounding material for matching.
// DomainID is the bare slug the frontend deep-links to
// (/space?panel=knowledge&domain=<DomainID>); Phrases are the verbatim
// snippets from that domain's injected grounding the matcher looks for in the
// model's spoken transcript. Mirrors realtime_output.py::GroundingDomain.
type citationDomain struct {
	DomainID string
	Phrases  []string
}

// CitationResolver derives structured citations from an assistant transcript
// by verbatim phrase matching against grounding domains. Matching is
// case-insensitive, but the emitted matchedPhrase is the substring AS IT
// APPEARS in the transcript so the frontend's exact indexOf wrap
// (splitTextAtCitations) finds it. The zero value (no domains) is the strict
// no-op resolver: it emits no citations, so an ungrounded realtime reply is
// byte-identical to an ungrounded text reply. Mirrors
// realtime_output.py::CitationResolver.
type CitationResolver struct {
	domains      []citationDomain
	minPhraseLen int
}

// NewCitationResolver builds a resolver from a #456 GroundingContext. Each
// fact's body is the citable material; its DomainName is the deep-link slug
// the matched phrase cites. Facts with no DomainName are not citable (nothing
// to attribute to) and are skipped, so a grounding context that carries only
// un-attributed graph facts yields the no-op resolver. Mirrors
// realtime_output.py::build_citation_resolver for the GroundingContext shape.
func NewCitationResolver(grounding GroundingContext) CitationResolver {
	byDomain := make(map[string][]string)
	var order []string
	for _, fact := range grounding.Facts {
		text := strings.TrimSpace(fact.Text)
		if text == "" {
			continue
		}
		domain := strings.TrimSpace(fact.DomainName)
		if domain == "" {
			continue
		}
		if _, seen := byDomain[domain]; !seen {
			order = append(order, domain)
		}
		byDomain[domain] = append(byDomain[domain], text)
	}
	domains := make([]citationDomain, 0, len(order))
	for _, domain := range order {
		domains = append(domains, citationDomain{DomainID: domain, Phrases: byDomain[domain]})
	}
	return CitationResolver{domains: domains, minPhraseLen: minCitationPhraseLen}
}

// IsEmpty reports whether the resolver has no grounding to match against (the
// no-op resolver). The capture path can skip citation work entirely when so.
func (r CitationResolver) IsEmpty() bool { return len(r.domains) == 0 }

// Resolve scans transcript for verbatim occurrences of each domain's grounding
// phrases and returns one AgentTurnCitation per distinct (domainId,
// matchedPhrase) hit. Longest phrases are tried first so a longer, more
// specific match wins before a shorter substring of it is considered.
// Returns nil (not an empty slice) when nothing matches so the caller treats
// "no citations" identically to the cascade's empty case.
func (r CitationResolver) Resolve(transcript string) []*memqlv1.AgentTurnCitation {
	if transcript == "" || len(r.domains) == 0 {
		return nil
	}
	minLen := r.minPhraseLen
	if minLen <= 0 {
		minLen = minCitationPhraseLen
	}
	haystack := strings.ToLower(transcript)

	type seenKey struct {
		domainID string
		phrase   string
	}
	seen := make(map[seenKey]struct{})
	var citations []*memqlv1.AgentTurnCitation

	for _, domain := range r.domains {
		domainID := strings.TrimSpace(domain.DomainID)
		if domainID == "" {
			continue
		}
		// Longest phrases first (stable) so the most specific match is preferred.
		phrases := append([]string(nil), domain.Phrases...)
		sort.SliceStable(phrases, func(i, j int) bool {
			return len(phrases[i]) > len(phrases[j])
		})
		for _, phrase := range phrases {
			phrase = strings.TrimSpace(phrase)
			if len([]rune(phrase)) < minLen {
				continue
			}
			idx := strings.Index(haystack, strings.ToLower(phrase))
			if idx < 0 {
				continue
			}
			// Emit the verbatim substring from the transcript (preserving the
			// transcript's casing) so the frontend's exact indexOf wrap
			// succeeds even when the grounding phrase differs in case.
			matched := transcript[idx : idx+len(phrase)]
			key := seenKey{domainID: domainID, phrase: matched}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			citations = append(citations, &memqlv1.AgentTurnCitation{
				DomainId:      domainID,
				MatchedPhrase: matched,
			})
		}
	}
	return citations
}
