package polyphon

import (
	"strings"
	"testing"
)

func TestExpandDomains(t *testing.T) {
	tests := []struct {
		name     string
		input    []string
		mustHave []string
	}{
		{
			name:     "IT expands to tech support variants",
			input:    []string{"IT"},
			mustHave: []string{"IT", "information technology", "tech support", "technical support"},
		},
		{
			name:     "engineering expands to development variants",
			input:    []string{"engineering"},
			mustHave: []string{"engineering", "development", "coding", "programming"},
		},
		{
			name:     "hr expands to human resources",
			input:    []string{"hr"},
			mustHave: []string{"hr", "human resources", "recruitment"},
		},
		{
			name:     "unknown domain preserved unchanged",
			input:    []string{"quantum physics"},
			mustHave: []string{"quantum physics"},
		},
		{
			name:     "no duplicates",
			input:    []string{"finance", "accounting"},
			mustHave: []string{"finance", "accounting", "bookkeeping"},
		},
		{
			name:     "empty input",
			input:    []string{},
			mustHave: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExpandDomains(tt.input)
			resultSet := make(map[string]bool)
			for _, r := range result {
				resultSet[strings.ToLower(r)] = true
			}
			for _, want := range tt.mustHave {
				if !resultSet[strings.ToLower(want)] {
					t.Errorf("expected %q in expanded domains, got: %v", want, result)
				}
			}
		})
	}

	t.Run("no duplicate entries", func(t *testing.T) {
		result := ExpandDomains([]string{"finance", "accounting"})
		seen := make(map[string]bool)
		for _, r := range result {
			lower := strings.ToLower(r)
			if seen[lower] {
				t.Errorf("duplicate entry: %s", r)
			}
			seen[lower] = true
		}
	})
}

func TestStemWord(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"troubleshooting", "troubleshoot"},
		{"questions", "question"},
		{"engineering", "engineer"},
		{"deployment", "deploy"},
		{"programming", "programm"},
		{"analytics", "analytic"},
		{"configurable", "configur"},
		{"debugging", "debugg"},
		// Short words should NOT be stemmed (remain >= 4 chars after strip).
		{"bus", "bus"},
		{"go", "go"},
		{"the", "the"},
		{"sing", "sing"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := stemWord(tt.input)
			if got != tt.want {
				t.Errorf("stemWord(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestStemmedDomainMatching(t *testing.T) {
	// Simulate what the improved keyword fallback will do:
	// "IT questions" should score > 0 against an agent with domain "information technology"
	// after synonym expansion.
	expanded := ExpandDomains([]string{"IT"})

	utteranceText := "it questions"
	utteranceWords := strings.Fields(strings.ToLower(utteranceText))

	matches := 0
	for _, domain := range expanded {
		domainLower := strings.ToLower(domain)
		for _, word := range utteranceWords {
			// Exact match or stemmed match.
			if word == domainLower || stemWord(word) == stemWord(domainLower) {
				matches++
				break
			}
			// Substring match for multi-word domains.
			if strings.Contains(utteranceText, domainLower) {
				matches++
				break
			}
		}
	}

	if matches == 0 {
		t.Errorf("expected at least one match for 'IT questions' against expanded IT domains: %v", expanded)
	}
}
