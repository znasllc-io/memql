package polyphon

import (
	"testing"
)

func TestSplitSentences(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: nil,
		},
		{
			name:     "whitespace only",
			input:    "   ",
			expected: nil,
		},
		{
			name:     "single sentence with period",
			input:    "Hello world.",
			expected: []string{"Hello world."},
		},
		{
			name:     "single sentence no punctuation",
			input:    "Hello world",
			expected: []string{"Hello world"},
		},
		{
			name:  "two sentences",
			input: "Hello world. How are you?",
			expected: []string{
				"Hello world.",
				"How are you?",
			},
		},
		{
			name:  "three sentences mixed punctuation",
			input: "I went to the store. Did you buy milk? That's amazing!",
			expected: []string{
				"I went to the store.",
				"Did you buy milk?",
				"That's amazing!",
			},
		},
		{
			name:  "abbreviation Mr",
			input: "Mr. Smith went to Washington. He arrived on time.",
			expected: []string{
				"Mr. Smith went to Washington.",
				"He arrived on time.",
			},
		},
		{
			name:  "abbreviation Dr",
			input: "Dr. Jones and Mrs. Williams met today. They discussed the plan.",
			expected: []string{
				"Dr. Jones and Mrs. Williams met today.",
				"They discussed the plan.",
			},
		},
		{
			name:  "decimal numbers",
			input: "The price is $3.99 per unit. We ordered 100 units.",
			expected: []string{
				"The price is $3.99 per unit.",
				"We ordered 100 units.",
			},
		},
		{
			name:  "e.g. abbreviation",
			input: "Use a framework, e.g. React or Vue. It will save time.",
			expected: []string{
				"Use a framework, e.g. React or Vue.",
				"It will save time.",
			},
		},
		{
			name:  "i.e. abbreviation",
			input: "The best option, i.e. the cheapest one, is clear. Let's go with it.",
			expected: []string{
				"The best option, i.e. the cheapest one, is clear.",
				"Let's go with it.",
			},
		},
		{
			name:     "quoted text not split",
			input:    `She said "Wait. Don't go." Then she left.`,
			expected: []string{`She said "Wait. Don't go." Then she left.`},
		},
		{
			name:  "short sentence merged with next",
			input: "Hi. How are you doing today?",
			expected: []string{
				"Hi. How are you doing today?",
			},
		},
		{
			name:  "multiple short sentences merge",
			input: "Oh. Wow. That is really interesting!",
			expected: []string{
				"Oh. Wow. That is really interesting!",
			},
		},
		{
			name:  "ellipsis",
			input: "Well... I'm not sure about that. Let me think.",
			expected: []string{
				"Well... I'm not sure about that.",
				"Let me think.",
			},
		},
		{
			name:  "exclamation marks",
			input: "That's incredible!! I can't believe it. Wow!",
			expected: []string{
				"That's incredible!!",
				"I can't believe it. Wow!",
			},
		},
		{
			name:     "no punctuation at end",
			input:    "This sentence has no ending punctuation",
			expected: []string{"This sentence has no ending punctuation"},
		},
		{
			name:  "mixed with no punctuation at end",
			input: "First sentence. Second sentence without ending",
			expected: []string{
				"First sentence.",
				"Second sentence without ending",
			},
		},
		{
			name:  "U.S.A. abbreviation",
			input: "He visited the U.S.A. last summer. It was great.",
			expected: []string{
				"He visited the U.S.A. last summer.",
				"It was great.",
			},
		},
		{
			name:  "long multi-sentence response",
			input: "Welcome to the platform! I'm Sofia, your AI assistant. I can help you with tours, surveys, and general questions. What would you like to explore today?",
			expected: []string{
				"Welcome to the platform!",
				"I'm Sofia, your AI assistant.",
				"I can help you with tours, surveys, and general questions.",
				"What would you like to explore today?",
			},
		},
		{
			name:  "question followed by statement",
			input: "How does this work? It uses a neural network to process input.",
			expected: []string{
				"How does this work?",
				"It uses a neural network to process input.",
			},
		},
		{
			name:  "etc at end",
			input: "Bring snacks, drinks, etc. We'll also need chairs.",
			expected: []string{
				"Bring snacks, drinks, etc. We'll also need chairs.",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SplitSentences(tt.input)

			if tt.expected == nil && result != nil {
				t.Errorf("expected nil, got %v", result)
				return
			}
			if tt.expected != nil && result == nil {
				t.Errorf("expected %v, got nil", tt.expected)
				return
			}

			if len(result) != len(tt.expected) {
				t.Errorf("expected %d sentences, got %d\nexpected: %v\ngot:      %v", len(tt.expected), len(result), tt.expected, result)
				return
			}

			for i := range tt.expected {
				if result[i] != tt.expected[i] {
					t.Errorf("sentence[%d] mismatch:\n  expected: %q\n  got:      %q", i, tt.expected[i], result[i])
				}
			}
		})
	}
}

func TestSplitSentences_SingleSentencePassthrough(t *testing.T) {
	// Single sentences should come through as-is.
	inputs := []string{
		"Just a simple statement.",
		"What time is it?",
		"Stop!",
		"A sentence without punctuation",
	}

	for _, input := range inputs {
		result := SplitSentences(input)
		if len(result) != 1 {
			t.Errorf("expected 1 sentence for %q, got %d: %v", input, len(result), result)
		}
		if len(result) == 1 && result[0] != input {
			t.Errorf("expected %q, got %q", input, result[0])
		}
	}
}
