package polyphon

import "strings"

// synonymMap maps canonical domain terms to their common synonyms and aliases.
// Used to expand agent domain lists so keyword matching catches related terms.
var synonymMap = map[string][]string{
	"it":              {"information technology", "tech support", "technical support", "helpdesk", "help desk"},
	"troubleshooting": {"debugging", "diagnosing", "fixing issues"},
	"engineering":     {"development", "coding", "programming", "software"},
	"design":          {"ux", "ui", "user experience", "user interface", "visual design"},
	"finance":         {"financial", "accounting", "bookkeeping", "budget"},
	"accounting":      {"bookkeeping", "finance", "financial"},
	"marketing":       {"advertising", "promotion", "branding", "campaign"},
	"hr":              {"human resources", "recruitment", "hiring", "people ops", "people operations"},
	"human resources": {"hr", "recruitment", "hiring", "people ops"},
	"security":        {"cybersecurity", "infosec", "information security"},
	"data":            {"analytics", "reporting", "metrics", "statistics"},
	"infrastructure":  {"devops", "cloud", "servers", "deployment", "ops"},
	"support":         {"help", "assistance", "customer service", "customer support"},
	"sales":           {"revenue", "deals", "pipeline", "business development"},
	"legal":           {"compliance", "regulatory", "contracts"},
}

// ExpandDomains returns the input domains plus common synonyms and aliases.
// Duplicates are removed. Original terms are always preserved.
func ExpandDomains(domains []string) []string {
	seen := make(map[string]bool, len(domains)*3)
	result := make([]string, 0, len(domains)*3)

	for _, d := range domains {
		lower := strings.ToLower(d)
		if !seen[lower] {
			seen[lower] = true
			result = append(result, d)
		}
		if synonyms, ok := synonymMap[lower]; ok {
			for _, syn := range synonyms {
				if !seen[syn] {
					seen[syn] = true
					result = append(result, syn)
				}
			}
		}
	}
	return result
}

// stemWord applies simple suffix stripping to normalize a word for matching.
// Only strips suffixes when the remaining stem is at least 4 characters long,
// preventing over-stripping of short words.
func stemWord(word string) string {
	// Ordered longest-first to match the most specific suffix.
	suffixes := []string{"tion", "ment", "ness", "able", "ible", "ing", "ies", "es", "ly", "er", "ed", "s"}
	for _, suffix := range suffixes {
		if len(word) > len(suffix)+3 && strings.HasSuffix(word, suffix) {
			return word[:len(word)-len(suffix)]
		}
	}
	return word
}
