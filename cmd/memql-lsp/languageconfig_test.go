package main

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

// The declarative editor-smarts file is hand-maintained (unlike the generated
// tmLanguage grammar). These tests pin the contract issue #2602 introduced:
// brace expansion and indentation must be driven by the language configuration
// itself, not by each user's editor.autoIndent setting.
const checkedInLanguageConfig = "../../editors/vscode/language-configuration.json"

type onEnterRule struct {
	BeforeText string `json:"beforeText"`
	AfterText  string `json:"afterText"`
	Action     struct {
		Indent string `json:"indent"`
	} `json:"action"`
}

type indentationRules struct {
	IncreaseIndentPattern string `json:"increaseIndentPattern"`
	DecreaseIndentPattern string `json:"decreaseIndentPattern"`
}

func loadLanguageConfig(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(checkedInLanguageConfig)
	if err != nil {
		t.Fatalf("read language configuration: %v", err)
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal language configuration: %v", err)
	}
	return cfg
}

// mustCompile guards that every pattern stays within RE2 syntax: VS Code
// accepts a wider dialect (lookaheads), but keeping the patterns RE2-safe
// keeps them testable here and portable to other editor hosts.
func mustCompile(t *testing.T, name, pattern string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("%s does not compile as RE2: %v", name, err)
	}
	return re
}

// TestLanguageConfigKeepsBaseKeys guards the pre-existing surface: adding the
// indentation smarts must not drop comments/brackets/autoClosingPairs.
func TestLanguageConfigKeepsBaseKeys(t *testing.T) {
	cfg := loadLanguageConfig(t)
	for _, key := range []string{"comments", "brackets", "autoClosingPairs", "surroundingPairs"} {
		if _, ok := cfg[key]; !ok {
			t.Errorf("language configuration lost key %q", key)
		}
	}
}

// TestLanguageConfigOnEnterRules: Enter between an unclosed `{` and its `}`
// must indent-outdent (the three-line expansion), and Enter after an unclosed
// `{` with no adjacent `}` must indent. The indentOutdent rule has to come
// first: rules are evaluated in order and the indent-only rule carries no
// afterText, so it would swallow the adjacent-brace case.
func TestLanguageConfigOnEnterRules(t *testing.T) {
	cfg := loadLanguageConfig(t)
	raw, ok := cfg["onEnterRules"]
	if !ok {
		t.Fatal("language configuration has no onEnterRules; brace expansion depends on the user's editor.autoIndent setting")
	}
	var rules []onEnterRule
	if err := json.Unmarshal(raw, &rules); err != nil {
		t.Fatalf("unmarshal onEnterRules: %v", err)
	}

	indentOutdentIdx, indentIdx := -1, -1
	for i, r := range rules {
		switch r.Action.Indent {
		case "indentOutdent":
			if indentOutdentIdx == -1 {
				indentOutdentIdx = i
			}
		case "indent":
			if indentIdx == -1 {
				indentIdx = i
			}
		}
	}
	if indentOutdentIdx == -1 {
		t.Fatal("no indentOutdent rule (Enter between {} would not expand)")
	}
	if indentIdx == -1 {
		t.Fatal("no indent rule (Enter after an unclosed { would not indent)")
	}
	if indentOutdentIdx > indentIdx {
		t.Fatalf("indentOutdent rule at %d ordered after indent rule at %d; the indent rule has no afterText and would swallow the adjacent-brace case", indentOutdentIdx, indentIdx)
	}

	between := rules[indentOutdentIdx]
	before := mustCompile(t, "indentOutdent beforeText", between.BeforeText)
	if between.AfterText == "" {
		t.Fatal("indentOutdent rule has no afterText; it would fire on every Enter after {")
	}
	after := mustCompile(t, "indentOutdent afterText", between.AfterText)
	for _, line := range []string{"args {", "  insert {", "mutation Space createSpace {"} {
		if !before.MatchString(line) {
			t.Errorf("indentOutdent beforeText does not match %q", line)
		}
	}
	if before.MatchString("args {}") {
		t.Error("indentOutdent beforeText matches a balanced-brace line \"args {}\"")
	}
	for _, line := range []string{"}", "  }"} {
		if !after.MatchString(line) {
			t.Errorf("indentOutdent afterText does not match %q", line)
		}
	}

	open := rules[indentIdx]
	openBefore := mustCompile(t, "indent beforeText", open.BeforeText)
	if !openBefore.MatchString("args {") {
		t.Error("indent beforeText does not match \"args {\"")
	}
}

// TestLanguageConfigIndentationRules: reindent (paste, format-on-type) must
// increase after a line that opens a `{` without closing it and decrease on a
// line that starts with `}`.
func TestLanguageConfigIndentationRules(t *testing.T) {
	cfg := loadLanguageConfig(t)
	raw, ok := cfg["indentationRules"]
	if !ok {
		t.Fatal("language configuration has no indentationRules; reindent depends on the user's editor.autoIndent setting")
	}
	var rules indentationRules
	if err := json.Unmarshal(raw, &rules); err != nil {
		t.Fatalf("unmarshal indentationRules: %v", err)
	}

	inc := mustCompile(t, "increaseIndentPattern", rules.IncreaseIndentPattern)
	dec := mustCompile(t, "decreaseIndentPattern", rules.DecreaseIndentPattern)

	for _, line := range []string{"args {", "  filter {", "automation dayRollup {"} {
		if !inc.MatchString(line) {
			t.Errorf("increaseIndentPattern does not match %q", line)
		}
	}
	for _, line := range []string{"args {}", "st := coalesce(args.stage, \"\")"} {
		if inc.MatchString(line) {
			t.Errorf("increaseIndentPattern matches %q, which opens no block", line)
		}
	}
	for _, line := range []string{"}", "  }", "\t}"} {
		if !dec.MatchString(line) {
			t.Errorf("decreaseIndentPattern does not match %q", line)
		}
	}
	if dec.MatchString("insert {") {
		t.Error("decreaseIndentPattern matches \"insert {\"")
	}
}
