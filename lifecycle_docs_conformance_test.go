package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestLifecycleDocsMatchRuling is the enforcement gate for the lifecycle
// ruling's documentation (#2609): absent = enabled, @enabled = accepted
// no-op, @disabled = the only off-switch, with real gates on every kind
// (#2604-#2608). The patterns target the drift SHAPE, not a list of known
// sites -- four review rounds proved that literal phrase lists get fitted
// to the sites already found and miss the next paraphrase.
//
// Like the product-neutrality gate, this polices GIT-TRACKED files only
// and sweeps the whole repo: a pre-ruling claim anywhere is a regression.
func TestLifecycleDocsMatchRuling(t *testing.T) {
	kinds := `(functions?|automations?|queries|mutations?|logic|tools?|builtins?|prompts?|seeds?|specs?|traits?|providers?|capabilit(y|ies)|actions?|definitions?|constructs?)`
	banned := []*regexp.Regexp{
		// "<kind> is/are ... disabled by default"
		regexp.MustCompile(`(?i)` + kinds + `\s+(are|is)[^.\n]{0,30}disabled by default`),
		// "@enabled ... to activate / to register / required / to run".
		// Backslash excluded from the gap so escaped newlines inside Go
		// string fixtures do not bridge lines.
		regexp.MustCompile(`(?i)@enabled` + "`?" + `[^.\n\\]{0,60}(to activate|to register|required)`),
		// "must use / flip to / add @enabled" and "required ... @enabled"
		regexp.MustCompile(`(?i)(must use|flip to|switch to)\s+` + "`?" + `@enabled`),
		regexp.MustCompile(`(?i)required[^.\n\\]{0,30}` + "`?" + `@enabled`),
		// the matrix/table row shapes reviews kept finding
		regexp.MustCompile(`required (to use it|for it to run)`),
		regexp.MustCompile(`all registered user-defined functions`),
	}

	skipDirs := map[string]bool{"bin": true, "node_modules": true, "testdata": true}
	const self = "lifecycle_docs_conformance_test.go"

	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || rel == self {
			continue
		}
		switch filepath.Ext(rel) {
		case ".md", ".go", ".memql":
		default:
			continue
		}
		skip := false
		for _, seg := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
			if skipDirs[seg] {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		data, err := os.ReadFile(rel)
		if err != nil {
			// git-tracked but locally absent (partial checkout); not repo drift.
			continue
		}
		content := string(data)
		for _, re := range banned {
			for _, loc := range re.FindAllStringIndex(content, -1) {
				line := 1 + strings.Count(content[:loc[0]], "\n")
				t.Errorf("%s:%d claims the pre-ruling lifecycle (%q); rewrite to the #2609 semantics", rel, line, content[loc[0]:loc[1]])
			}
		}
	}
}
