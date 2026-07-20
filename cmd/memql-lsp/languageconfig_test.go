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
	BeforeText       string `json:"beforeText"`
	AfterText        string `json:"afterText"`
	PreviousLineText string `json:"previousLineText"`
	Action           struct {
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
//
// Known limit shared by every per-line regex approach: lines INSIDE a
// /* ... */ block comment cannot be excluded declaratively (the corpus uses
// // comments exclusively, so this is theoretical today).
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

// winningEnterAction mirrors VS Code's evaluation (onEnter.ts): rules are
// walked in order and the FIRST rule whose beforeText matches (and whose
// afterText, when present, also matches) wins. Pinning the winner rather than
// the rules' relative order catches an unreachable rule - e.g. a catch-all
// prepended above the pair - which order checks alone are blind to.
func winningEnterAction(t *testing.T, rules []onEnterRule, previous, before, after string) string {
	t.Helper()
	for _, r := range rules {
		if !mustCompile(t, "beforeText", r.BeforeText).MatchString(before) {
			continue
		}
		if r.AfterText != "" && !mustCompile(t, "afterText", r.AfterText).MatchString(after) {
			continue
		}
		if r.PreviousLineText != "" && !mustCompile(t, "previousLineText", r.PreviousLineText).MatchString(previous) {
			continue
		}
		return r.Action.Indent
	}
	return ""
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
	if indentOutdentIdx != 0 {
		t.Fatalf("indentOutdent rule at index %d, want 0; any earlier rule can shadow it (first match wins)", indentOutdentIdx)
	}
	if indentOutdentIdx > indentIdx {
		t.Fatalf("indentOutdent rule at %d ordered after indent rule at %d; the indent rule has no afterText and would swallow the adjacent-brace case", indentOutdentIdx, indentIdx)
	}

	// First-match-wins semantics: the WINNING action for each scenario, not
	// just the presence/order of rules.
	for _, tc := range []struct {
		before, after, want string
	}{
		{"args {", "}", "indentOutdent"},
		{"args {", "  }", "indentOutdent"},
		{"args {", "", "indent"},
		{"args { // trailing comment", "", "indent"},
		{"args {", "name: String", "indent"},
		// Slash-bearing openers: the comment guard must anchor on the first
		// non-space character, not ban '/' anywhere before the brace.
		{"note string @description(\"n/a\") {", "", "indent"},
		{"if total / count > 5 {", "", "indent"},
		// Paren and bracket blocks (#2638): the multi-line call-args idiom
		// (dsl/cluster/automations.memql) and list literals must expand and
		// indent exactly like braces.
		{"logic bootstrapCluster (", ")", "indentOutdent"},
		{"      mutation createDatabase (", "", "indent"},
		{"tags: [", "]", "indentOutdent"},
		{"tags: [", "", "indent"},
	} {
		if got := winningEnterAction(t, rules, "", tc.before, tc.after); got != tc.want {
			t.Errorf("winning action for (%q, %q) = %q, want %q", tc.before, tc.after, got, tc.want)
		}
	}
	// No rule may fire at all on these: balanced pairs and commented-out code.
	for _, before := range []string{"args {}", "// args {", "  // automation x {", "cond(a > 0)", "tags: [\"a\", \"b\"]", "// mutation createDatabase ("} {
		if got := winningEnterAction(t, rules, "", before, ""); got != "" {
			t.Errorf("winning action for (%q, \"\") = %q, want no rule to fire", before, got)
		}
	}

	between := rules[indentOutdentIdx]
	if between.AfterText == "" {
		t.Fatal("indentOutdent rule has no afterText; it would fire on every Enter after {")
	}
	before := mustCompile(t, "indentOutdent beforeText", between.BeforeText)
	for _, line := range []string{"args {", "  insert {", "mutation Space createSpace {", "@variant(discriminator=\"kind\") {", "logic bootstrapCluster (", "tags: ["} {
		if !before.MatchString(line) {
			t.Errorf("indentOutdent beforeText does not match %q", line)
		}
	}
	for _, line := range []string{"args {}", "cond(a > 0)"} {
		if before.MatchString(line) {
			t.Errorf("indentOutdent beforeText matches balanced-pair line %q", line)
		}
	}

	open := rules[indentIdx]
	openBefore := mustCompile(t, "indent beforeText", open.BeforeText)
	if !openBefore.MatchString("args {") {
		t.Error("indent beforeText does not match \"args {\"")
	}
	if openBefore.MatchString("args {}") {
		t.Error("indent beforeText matches a balanced-brace line \"args {}\"; Enter after it would mis-indent")
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

	for _, line := range []string{"args {", "  filter {", "automation dayRollup {", "args { // trailing comment", "note string @description(\"n/a\") {", "if total / count > 5 {", "logic bootstrapCluster (", "      mutation createDatabase (", "tags: ["} {
		if !inc.MatchString(line) {
			t.Errorf("increaseIndentPattern does not match %q", line)
		}
	}
	for _, line := range []string{"args {}", "st := coalesce(args.stage, \"\")", "// args {", "  // automation x {", "cond(a > 0)", "tags: [\"a\", \"b\"]", "// mutation createDatabase ("} {
		if inc.MatchString(line) {
			t.Errorf("increaseIndentPattern matches %q, which opens no block", line)
		}
	}
	for _, line := range []string{"}", "  }", "\t}", ")", "  )", "\t)", "]", "  ],", "),"} {
		if !dec.MatchString(line) {
			t.Errorf("decreaseIndentPattern does not match %q", line)
		}
	}
	// The anchor matters: an unanchored pattern would outdent any line merely
	// CONTAINING a closer (balanced object literals, closers mid-line).
	for _, line := range []string{"insert {", "x: { a: 1 }", "label: \"}\"", "st := coalesce(args.stage, \"\")", "x: [1, 2]"} {
		if dec.MatchString(line) {
			t.Errorf("decreaseIndentPattern matches %q, which does not close a block", line)
		}
	}
}
