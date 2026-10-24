package harness

import (
	"errors"
	"strings"
	"testing"
)

// levels_test.go pins the translation a LEVEL gets on this machine, per app
// and per level (memql-cockpit#437's acceptance), and the refusals that keep
// a session from running at a level nobody chose.
//
// The built-in tables are asserted VALUE BY VALUE rather than "is non-empty",
// because every value in them is a claim about an installed binary -- an
// alias Claude Code resolves, an effort word Codex's models advertise -- and
// a change to one is a change to what somebody's subscription is spent on.
// Changing a row here should be a decision somebody makes on purpose.

func TestBuiltinLevelsClaudeCode(t *testing.T) {
	want := Table{
		"fast":      {Model: "haiku"},
		"strong":    {Model: "sonnet", Effort: "high"},
		"reasoning": {Model: "opus", Effort: "xhigh"},
	}
	got := BuiltinLevels(HarnessClaudeHeadless)
	if len(got) != len(want) {
		t.Fatalf("claude-code table = %v, want %v", got, want)
	}
	for level, k := range want {
		if got[level] != k {
			t.Errorf("claude-code %s = %+v, want %+v", level, got[level], k)
		}
		// The built-in table must pass the check an owner's entry has to
		// pass: a default the cockpit would refuse from a policy file is a
		// default nobody can reproduce by writing it down.
		if err := CheckKnobs(HarnessClaudeHeadless, got[level]); err != nil {
			t.Errorf("the built-in claude-code %s entry fails its own check: %v", level, err)
		}
	}
}

// Codex names no model, only an effort. Its catalogue is per account and its
// names change between releases, so a model spelled here would be a claim
// this machine cannot keep for anybody else's account.
func TestBuiltinLevelsCodexSetsEffortNotModel(t *testing.T) {
	want := Table{
		"fast":      {Effort: "low"},
		"strong":    {Effort: "medium"},
		"reasoning": {Effort: "high"},
	}
	for _, word := range []string{HarnessCodexAppServer, HarnessCodexMCP} {
		got := BuiltinLevels(word)
		if len(got) != len(want) {
			t.Fatalf("%s table = %v, want %v", word, got, want)
		}
		for level, k := range want {
			if got[level] != k {
				t.Errorf("%s %s = %+v, want %+v", word, level, got[level], k)
			}
			if err := CheckKnobs(word, got[level]); err != nil {
				t.Errorf("the built-in %s %s entry fails its own check: %v", word, level, err)
			}
		}
	}
}

func TestEveryAppLevelHasABuiltinEntryAndEmbeddingsHasNone(t *testing.T) {
	levels := AppLevels()
	if strings.Join(levels, ",") != "fast,strong,reasoning" {
		t.Fatalf("AppLevels = %v, want fast, strong, reasoning in the order a person reads them", levels)
	}
	for _, word := range []string{HarnessClaudeHeadless, HarnessCodexAppServer, HarnessCodexMCP} {
		table := BuiltinLevels(word)
		for _, level := range levels {
			if _, ok := table[level]; !ok {
				t.Errorf("%s has no built-in entry for %s", word, level)
			}
		}
		if _, ok := table["embeddings"]; ok {
			t.Errorf("%s has an embeddings entry; no app serves one (D10)", word)
		}
	}
	if BuiltinLevels("telepathy") != nil {
		t.Error("a harness this cockpit does not have has no table, so every level refuses on it")
	}
}

func TestResolveLevel(t *testing.T) {
	table := BuiltinLevels(HarnessClaudeHeadless)

	if k, err := ResolveLevel("", table); err != nil || k != (Knobs{}) {
		t.Errorf("no level = %+v, %v; want no knobs and no error -- the app's own defaults", k, err)
	}
	if k, err := ResolveLevel("reasoning", table); err != nil || k != (Knobs{Model: "opus", Effort: "xhigh"}) {
		t.Errorf("reasoning = %+v, %v", k, err)
	}
	if _, err := ResolveLevel("embeddings", table); !errors.Is(err, ErrEmbeddingsLevel) {
		t.Errorf("embeddings err = %v, want ErrEmbeddingsLevel", err)
	}

	_, err := ResolveLevel("turbo", table)
	if err == nil || !strings.Contains(err.Error(), "fast, strong, reasoning, embeddings") {
		t.Errorf("unknown level err = %v, want the engine's own sentence naming the four levels", err)
	}
	// A level is spelled exactly, as the engine's ParseLevel spells it.
	if _, err := ResolveLevel("Fast", table); err == nil {
		t.Error(`"Fast" is not "fast"`)
	}
	// A defined level the table has no entry for refuses rather than
	// running at the app's defaults: an owner who wrote a table meant it.
	if _, err := ResolveLevel("fast", Table{}); err == nil {
		t.Error("a level with no translation must refuse")
	}
}

func TestCheckAppLevel(t *testing.T) {
	for _, level := range AppLevels() {
		if err := CheckAppLevel(level); err != nil {
			t.Errorf("CheckAppLevel(%q) = %v, want nil", level, err)
		}
	}
	if err := CheckAppLevel("embeddings"); !errors.Is(err, ErrEmbeddingsLevel) {
		t.Errorf("CheckAppLevel(embeddings) = %v, want ErrEmbeddingsLevel", err)
	}
	if err := CheckAppLevel(""); err == nil {
		t.Error("an empty word is not a level an entry can name")
	}
}

func TestCheckKnobs(t *testing.T) {
	cases := []struct {
		name string
		word string
		k    Knobs
		ok   bool
	}{
		{"claude alias with a context suffix", HarnessClaudeHeadless, Knobs{Model: "sonnet[1m]", Effort: "max"}, true},
		{"claude full model name", HarnessClaudeHeadless, Knobs{Model: "claude-fable-5-1"}, true},
		{"claude effort it would ignore", HarnessClaudeHeadless, Knobs{Effort: "extreme"}, false},
		{"claude effort in the wrong case", HarnessClaudeHeadless, Knobs{Effort: "High"}, false},
		{"a model that is a flag", HarnessClaudeHeadless, Knobs{Model: "-p"}, false},
		{"a model with a space", HarnessClaudeHeadless, Knobs{Model: "claude opus"}, false},
		{"codex model and an advertised effort", HarnessCodexAppServer, Knobs{Model: "gpt-5.5", Effort: "ultra"}, true},
		{"codex effort that is not a word", HarnessCodexMCP, Knobs{Effort: "High!"}, false},
		{"nothing at all", HarnessCodexMCP, Knobs{}, true},
		{"a harness nobody has", "telepathy", Knobs{Effort: "low"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckKnobs(c.word, c.k)
			if (err == nil) != c.ok {
				t.Errorf("CheckKnobs(%s, %+v) = %v, want ok=%v", c.word, c.k, err, c.ok)
			}
		})
	}

	// Claude Code IGNORES an effort it does not know, with a line on
	// stderr, rather than refusing it -- so the refusal here is the only
	// one anybody will see, and it has to name the words that work.
	err := CheckKnobs(HarnessClaudeHeadless, Knobs{Effort: "extreme"})
	if err == nil || !strings.Contains(err.Error(), "low, medium, high, xhigh, max") {
		t.Errorf("the refusal must name Claude Code's own words: %v", err)
	}
}

func TestMergeLevelsReplacesWholeEntries(t *testing.T) {
	got := MergeLevels(BuiltinLevels(HarnessClaudeHeadless), Table{"strong": {Model: "opus"}})
	if got["strong"] != (Knobs{Model: "opus"}) {
		t.Errorf("strong = %+v; an owner's entry replaces the built-in one WHOLE, "+
			"so the built-in effort must not ride along with the owner's model", got["strong"])
	}
	if got["fast"] != (Knobs{Model: "haiku"}) {
		t.Errorf("fast = %+v; a level the owner did not list keeps the built-in entry", got["fast"])
	}
	if MergeLevels(nil, nil) == nil {
		t.Error("the merge of nothing is an empty table, not nil")
	}
}

func TestDescribeKnobs(t *testing.T) {
	cases := []struct {
		word string
		k    Knobs
		want string
	}{
		{HarnessClaudeHeadless, Knobs{Model: "opus", Effort: "xhigh"}, "--model opus --effort xhigh"},
		{HarnessClaudeHeadless, Knobs{Model: "haiku"}, "--model haiku"},
		{HarnessCodexAppServer, Knobs{Effort: "low"}, "model_reasoning_effort=low, on the account's default model"},
		{HarnessCodexMCP, Knobs{Model: "gpt-5.5", Effort: "high"}, "model gpt-5.5, model_reasoning_effort=high"},
		{HarnessCodexMCP, Knobs{Model: "gpt-5.5"}, "model gpt-5.5, at the model's default effort"},
		{HarnessClaudeHeadless, Knobs{}, "the app's own defaults"},
	}
	for _, c := range cases {
		if got := DescribeKnobs(c.word, c.k); got != c.want {
			t.Errorf("DescribeKnobs(%s, %+v) = %q, want %q", c.word, c.k, got, c.want)
		}
	}
}

func TestSpecKnobs(t *testing.T) {
	// Nil Levels is the built-in table for the harness.
	k, err := Spec{Level: "strong"}.knobs(HarnessClaudeHeadless)
	if err != nil || k != (Knobs{Model: "sonnet", Effort: "high"}) {
		t.Errorf("built-in strong = %+v, %v", k, err)
	}
	// A table the caller passed wins over the built-in one.
	k, err = Spec{Level: "strong", Levels: Table{"strong": {Model: "opus"}}}.knobs(HarnessClaudeHeadless)
	if err != nil || k != (Knobs{Model: "opus"}) {
		t.Errorf("passed-in strong = %+v, %v", k, err)
	}
	// A table carrying a value the app would misread never reaches it.
	if _, err := (Spec{Level: "fast", Levels: Table{"fast": {Effort: "extreme"}}}).knobs(HarnessClaudeHeadless); err == nil {
		t.Error("an effort Claude Code would ignore reached the harness")
	}
}

func TestExplicitSessionKnobsOverrideDefaultsWithoutChangingOtherSessions(t *testing.T) {
	for _, word := range []string{HarnessCodexAppServer, HarnessCodexMCP, HarnessClaudeHeadless} {
		t.Run(word, func(t *testing.T) {
			table := BuiltinLevels(word)
			before, _ := ResolveLevel("strong", table)
			s := Spec{Level: "strong", Levels: table, Model: "test-balanced-model", Effort: "medium"}
			got, err := s.knobs(word)
			if err != nil || got.Model != s.Model || got.Effort != "medium" {
				t.Fatalf("override lost: %+v %v", got, err)
			}
			after, _ := ResolveLevel("strong", table)
			if after != before {
				t.Fatal("session changed shared defaults")
			}
			s.Level = ""
			if got, err = s.knobs(word); err != nil || got.Model != s.Model {
				t.Fatalf("pin without level lost: %+v %v", got, err)
			}
			s.Effort = "High!"
			if _, err = s.knobs(word); err == nil {
				t.Fatal("invalid explicit effort silently fell back")
			}
			s.Effort = "medium"
			s.ResumeRef = "existing"
			if word == HarnessCodexMCP {
				if _, err = s.knobs(word); err == nil {
					t.Fatal("unconfigurable resume silently lost override")
				}
			}
		})
	}
}
