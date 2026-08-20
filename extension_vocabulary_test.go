package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// This file gates the component / integration / pack vocabulary lock
// (docs/public/concepts/component-integration-pack.md, established by
// memql#4135 and finished by epic memql#4161). The lock's failure mode is
// silent: "plugin" is ordinary, confident English, so a sentence calling a
// pack "the product's plugin module" or the VS Code extension "the plugin"
// reads as fine prose while reintroducing exactly the fourth extension
// kind the canonical doc forbids. Before this gate existed, the lock was
// established in one commit and its vocabulary regressed nowhere it was
// propagated -- and survived everywhere it was not (the epic's survey
// found seven conflation sites two weeks after the lock landed).
//
// # Probe-first, not pattern-first (house rule)
//
// Per the canonical shape of a root gate (see the long comments on
// TestNoDatabaseProductClaims and TestRetiredVocabulary): every pattern
// below was run over the REAL tree and its full hit list personally
// triaged before being frozen here. At freeze time (post-#4162..#4165):
//   - the prose patterns hit exactly one file,
//     docs/internal/design/voice-432-conductor-response-gate.md:295, which
//     is `status: historical` and therefore exempt (and is also about a
//     Python plugin class in a retired design, not memQL vocabulary);
//   - the editors/vscode source pattern hit nothing (the sweep in this
//     same change removed the last ~30 comment mentions).
//
// # What this gate deliberately does NOT catch
//
//   - Proper nouns and identifiers: "Plugin SDK v1" (the SDK's title),
//     the Go primitives RegisterPlugin / RegisterPluginForContract and
//     their types (PluginContext, PluginContractVersion, ...), and file
//     paths like plugin-sdk.md or integrations/<name>/plugin.go. Those
//     name MECHANISM, not a category of artifact, and the canonical doc
//     keeps them. The prose patterns are phrase-shaped precisely so these
//     never match.
//   - Bare "plugin" in prose outside editors/vscode. A bare-word ban
//     would flag every legitimate mention of the SDK title and the
//     primitives; the phrase patterns catch the two regression shapes the
//     survey actually found ("product(-repo) plugin", "plugin
//     module/repo") and nothing else. Extend the list ONLY with patterns
//     whose full current-tree hit list you have personally triaged.
//   - Go source comments. #4165 converged component/memql/plugins.go and
//     app/plugins.go by hand; sweeping every .go comment for the word
//     would flag the primitives' own doc comments. Review owns that tree.
//
// # Escape hatches
//
//   - Reword to the locked vocabulary ("pack", "the extension") -- the fix
//     in almost every case.
//   - A `status: historical` doc is exempt wholesale (rationale is the
//     value; DOCS_STANDARD forbids rewriting it).
//   - A legitimate discussion of the retired word carries the same
//     per-line marker TestRetiredVocabulary uses:
//     <!-- retired-vocabulary-ok: reason -->. Mark the line, never the
//     file.

// extensionVocabulary are the phrase shapes that reintroduce "plugin" as a
// category noun for a pack. See the file comment for the probe record and
// the extend-only-after-triage rule.
var extensionVocabulary = []struct{ pattern, reason, ref string }{
	{`(?i)product(-repo)? plugin`, `a product's Go extension is a pack, not a "product plugin"`, "memql#4162"},
	{`(?i)\bplugin (module|repo)\b`, `the downstream Go module is a pack module; a client repo is not a "plugin repo"`, "memql#4162"},
}

// extensionVocabScopeFiles enumerates every tracked markdown file except
// docs/superpowers/** (specs and plans quote banned forms by design) and
// the machine-generated reference tree. Wider than vocabScopeFiles on
// purpose: the survey's conflation sites were mostly in CLAUDE.md files
// and docs/internal, which the docs/public-scoped gate cannot see.
func extensionVocabScopeFiles() ([]string, error) {
	out, err := exec.Command("git", "ls-files", "-z", "--", "*.md",
		":!docs/superpowers", ":!docs/public/reference/_generated").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	var files []string
	for _, f := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// TestPluginMeansPackVocabulary fails when prose reintroduces "plugin" as
// a category noun for what the vocabulary lock calls a pack.
func TestPluginMeansPackVocabulary(t *testing.T) {
	compiled := make([]*regexp.Regexp, len(extensionVocabulary))
	for i, ev := range extensionVocabulary {
		compiled[i] = regexp.MustCompile(ev.pattern)
	}

	scope, err := extensionVocabScopeFiles()
	if err != nil {
		t.Fatalf("enumerate scope: %v", err)
	}
	for _, file := range scope {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		content := string(data)
		if status, ok := frontMatterStatus(content); ok && status == "historical" {
			continue
		}
		for i, line := range strings.Split(content, "\n") {
			if lineIsVocabExempt(line) {
				continue
			}
			for j, re := range compiled {
				if re.MatchString(line) {
					ev := extensionVocabulary[j]
					t.Errorf("%s:%d matches `%s` (%s, %s):\n  %s",
						file, i+1, ev.pattern, ev.reason, ev.ref, strings.TrimSpace(line))
				}
			}
		}
	}
}

// TestVsCodeExtensionSourceHasNoPlugin fails when editors/vscode source or
// test code calls the extension "plugin" in any spelling. Unlike the prose
// gate this IS a bare-word ban: after the memql#4166 sweep the extension's
// own tree has zero occurrences, no proper noun lives there, and VS Code's
// marketplace word is "extension" -- so any new occurrence is a regression,
// not a judgment call. editors/vscode/README.md is deliberately OUT of this
// scope: it states the naming convention and must be able to name the
// forbidden word.
func TestVsCodeExtensionSourceHasNoPlugin(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "-z", "--",
		"editors/vscode/src", "editors/vscode/test").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	re := regexp.MustCompile(`(?i)\bplugin\b`)
	for _, file := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if file == "" {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				t.Errorf("%s:%d calls the extension \"plugin\" -- the word is \"extension\" (memql#4163, memql#4166):\n  %s",
					file, i+1, strings.TrimSpace(line))
			}
		}
	}
}
