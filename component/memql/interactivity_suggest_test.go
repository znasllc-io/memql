package memql

import "testing"

func TestSuggestInteractivity(t *testing.T) {
	cases := []struct {
		name string
		goal string
		want string
	}{
		// Pedagogical -> conversational
		{"walk me through agent creation", "walk me through creating an agent", "conversational"},
		{"teach me", "teach me how to set up my profile", "conversational"},
		{"show me how", "show me how to invite a guest", "conversational"},
		{"explain how", "explain how spaces work in CoPresent", "conversational"},
		{"guide me", "guide me through onboarding", "conversational"},
		{"help me set up", "help me set up my notification preferences", "conversational"},
		{"step by step", "delete the agent step by step", "conversational"},
		{"how do I", "how do I create a polyphon space", "conversational"},
		{"tutorial", "give me a tutorial of the app", "conversational"},

		// Transactional -> minimal
		{"set theme dark", "set the theme to dark", "minimal"},
		{"change cursor speed", "change cursor speed to max", "minimal"},
		{"switch to dark mode", "switch to dark mode", "minimal"},
		{"delete agent", "delete the agent named Test", "minimal"},
		{"open spaces menu", "open the Spaces menu", "minimal"},
		{"show me the version", "show me the CoPresent version", "minimal"}, // info-intent reads as minimal
		{"navigate to settings", "navigate to settings", "minimal"},
		{"please prefix", "please change the theme to dark", "minimal"},

		// Ambiguous / multi-field -> no suggestion (let agent decide)
		{"create agent IT support", "create an agent for IT support", ""},
		{"edit Astra (no field)", "edit Astra", ""},
		{"create space (no title)", "create a space", ""},
		{"configure notifications", "configure my notification preferences", ""},
		{"empty goal", "", ""},
		{"only whitespace", "   ", ""},

		// Pedagogical wins ties
		{"teach + transactional verbs", "teach me how to delete an agent", "conversational"},
		{"walk through + change", "walk me through changing the theme", "conversational"},

		// False-positive guards
		{"show me the X is not 'show me how'", "show me the version number", "minimal"}, // matches `show me` transactional, not pedagogical
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SuggestInteractivity(tc.goal)
			if got != tc.want {
				t.Errorf("SuggestInteractivity(%q) = %q, want %q", tc.goal, got, tc.want)
			}
		})
	}
}
