package id

import "testing"

func TestSlugify(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"already kebab", "row-crop-farmer", "row-crop-farmer"},
		{"snake to kebab", "row_crop_farmer", "row-crop-farmer"},
		{"title case", "Row Crop Farmer", "row-crop-farmer"},
		{"mixed separators", "row crop_farmer.v2", "row-crop-farmer-v2"},
		{"path-style", "agents/roles/row_crop", "agents-roles-row-crop"},
		{"collapse runs", "foo___bar", "foo-bar"},
		{"collapse dashes", "foo---bar", "foo-bar"},
		{"trim leading", "---farmer", "farmer"},
		{"trim trailing", "farmer---", "farmer"},
		{"trim both", "  --farmer--  ", "farmer"},
		{"digits", "agent42", "agent42"},
		{"digits in middle", "agent_42_thing", "agent-42-thing"},
		{"drop punctuation", "row's farmer!", "rows-farmer"},
		{"drop non-ascii", "ümlaut", "mlaut"},
		{"only separators", "___", ""},
		{"only punctuation", "!!!", ""},
		{"system_planner", "system_planner", "system-planner"},
		{"camelCase split", "plannerAgent", "planner-agent"},
		{"camelCase three words", "trainerAgentReply", "trainer-agent-reply"},
		{"acronym then word (no split between caps)", "HTTPServer", "httpserver"},
		{"leading uppercase", "Planner", "planner"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Slugify(tc.in)
			if got != tc.want {
				t.Errorf("Slugify(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSlugifyDeterministic(t *testing.T) {
	// Same input should always produce the same output -- catalog rows
	// rely on this for stable ids across boots.
	want := Slugify("Row Crop Farmer")
	for i := 0; i < 100; i++ {
		if got := Slugify("Row Crop Farmer"); got != want {
			t.Fatalf("Slugify is non-deterministic: iter %d got %q, want %q", i, got, want)
		}
	}
}
