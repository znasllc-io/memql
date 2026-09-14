package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// The VS Code extension declares the language it was built from, and this
// file is the engine-side half of that promise (memql#5362; D25 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// The extension ships its own language server, so its completion and
// diagnostics are the grammar of the commit it was packaged from. A cluster is
// the grammar of ITS commit. Nothing tied the two together: a grammar change
// could merge with no extension release carrying it, and an editor would then
// go on offering yesterday's forms against a cluster that refuses them, with
// nothing anywhere saying so.
//
// So `editors/vscode/package.json` carries a top-level
//
//	"memql": {"edition": "<parser.Edition>", "grammarVersion": "<parser.GrammarVersion>"}
//
// and parser.EditorRelease names the first extension release that carries
// GrammarVersion -- the release a cluster tells an older editor to install.
// This gate fails when the parser moved and the pin, the version or the
// changelog did not move with it, so a grammar change cannot merge without the
// extension release that carries it. Every message names the exact edit.
//
// What it cannot see, and says in its messages rather than pretending to: it
// is hermetic, so it cannot know whether parser.EditorRelease has already been
// PUBLISHED. Before that release ships, moving the grammar means editing its
// pin and its changelog line; after it ships, the grammar needs a new release.
// The grammar-mismatch message spells out both.
const checkedInExtensionChangelog = "../../editors/vscode/CHANGELOG.md"

// extensionLanguageManifest reads only the two manifest facts this gate
// judges. Its own type rather than fields on extensionManifest, for the reason
// extensionContributions gives: that struct is built literally by other
// fixtures, and widening it would break them for fields only this gate reads.
//
// MemQL is a pointer so that a manifest with no "memql" block at all reads
// differently from one whose block is present and empty: they need different
// edits, and the message should name the right one.
type extensionLanguageManifest struct {
	Version string `json:"version"`
	MemQL   *struct {
		Edition        string `json:"edition"`
		GrammarVersion string `json:"grammarVersion"`
	} `json:"memql"`
}

// editorParityInput is everything the gate judges, as plain values, so the
// decision can be driven by synthetic inputs as well as by the real tree.
type editorParityInput struct {
	// The engine's side: parser.Edition, parser.GrammarVersion and
	// parser.EditorRelease.
	Edition, GrammarVersion, EditorRelease string
	// The extension's side, read from editors/vscode.
	Manifest  extensionLanguageManifest
	Changelog string
}

// editorParity returns one problem per drift, each naming the edit that ends
// it. Empty means the extension and the parser agree.
func editorParity(in editorParityInput) []string {
	var problems []string
	release := in.EditorRelease

	// Written once, because two checks point at it: after release has shipped,
	// fixing the pin in place would claim a published extension carries a
	// grammar it was never built with.
	ifPublished := fmt.Sprintf(
		"If %[1]s has already been published, the change needs a new release instead: raise "+
			"parser.EditorRelease (component/language/parser/edition.go) and \"version\" in "+
			"editors/vscode/package.json to that release, and give editors/vscode/CHANGELOG.md a "+
			"\"## <release>\" section naming %[2]q and \"edition %[3]s\".",
		release, in.GrammarVersion, in.Edition)

	// A stale pin's message names the changelog edit as part of the whole fix,
	// so the changelog checks below do not report the same edit a second time.
	pinGrammarStale, pinEditionStale := false, false
	switch pin := in.Manifest.MemQL; {
	case pin == nil:
		problems = append(problems, fmt.Sprintf(
			"editors/vscode/package.json has no top-level \"memql\" block, so the extension does not say "+
				"which language it was built from. Add \"memql\": {\"edition\": %q, \"grammarVersion\": %q}.",
			in.Edition, in.GrammarVersion))
	default:
		if pin.GrammarVersion != in.GrammarVersion {
			pinGrammarStale = true
			problems = append(problems, fmt.Sprintf(
				"editors/vscode/package.json pins \"memql\": {\"grammarVersion\": %[1]q}, but "+
					"parser.GrammarVersion is %[2]q. A grammar change merges together with the extension "+
					"release that carries it, and the whole fix is three edits:\n"+
					"  1. editors/vscode/package.json: set \"memql\": {\"grammarVersion\": %[2]q}.\n"+
					"  2. editors/vscode/CHANGELOG.md: name %[2]q in the \"## %[3]s\" section, in place of %[1]q.\n"+
					"  3. Run `make vscode-grammar`, which regenerates the TextMate grammar "+
					"(editors/vscode/syntaxes/memql.tmLanguage.json) and the language configuration.\n%[4]s",
				pin.GrammarVersion, in.GrammarVersion, release, ifPublished))
		}
		if pin.Edition != in.Edition {
			pinEditionStale = true
			problems = append(problems, fmt.Sprintf(
				"editors/vscode/package.json pins \"memql\": {\"edition\": %[1]q}, but parser.Edition is %[2]q. "+
					"Set \"memql\": {\"edition\": %[2]q} and say \"edition %[2]s\" in the \"## %[3]s\" section of "+
					"editors/vscode/CHANGELOG.md.\n%[4]s",
				pin.Edition, in.Edition, release, ifPublished))
		}
	}

	releaseParts, releaseOK := parseExtensionRelease(release)
	if !releaseOK {
		problems = append(problems, fmt.Sprintf(
			"parser.EditorRelease is %q, which is not a MAJOR.MINOR.PATCH release; write it as one "+
				"(e.g. \"0.4.0\") in component/language/parser/edition.go.", release))
	}
	versionParts, versionOK := parseExtensionRelease(in.Manifest.Version)
	if !versionOK {
		problems = append(problems, fmt.Sprintf(
			"editors/vscode/package.json has \"version\": %q, which is not a MAJOR.MINOR.PATCH release; "+
				"the publish lane tags it as memql-vscode-v<version>, so write it as one.", in.Manifest.Version))
	}
	if releaseOK && versionOK && compareExtensionReleases(releaseParts, versionParts) > 0 {
		problems = append(problems, fmt.Sprintf(
			"parser.EditorRelease is %[1]q, but editors/vscode/package.json is at \"version\": %[2]q: the "+
				"first release that carries GrammarVersion %[3]q cannot be newer than the extension being "+
				"built. Set \"version\": %[1]q in editors/vscode/package.json.",
			release, in.Manifest.Version, in.GrammarVersion))
	}

	section, found := changelogSection(in.Changelog, release)
	switch {
	case !found:
		problems = append(problems, fmt.Sprintf(
			"editors/vscode/CHANGELOG.md has no \"## %[1]s\" section, and parser.EditorRelease names %[1]s "+
				"as the first release that carries GrammarVersion %[2]q. Add a \"## %[1]s\" section that names "+
				"the grammar (%[2]q) and \"edition %[3]s\".",
			release, in.GrammarVersion, in.Edition))
	default:
		if !pinGrammarStale && !strings.Contains(section, in.GrammarVersion) {
			problems = append(problems, fmt.Sprintf(
				"the \"## %[1]s\" section of editors/vscode/CHANGELOG.md does not name GrammarVersion %[2]q. "+
					"Write %[2]q into it: %[1]s is the release that carries that grammar.",
				release, in.GrammarVersion))
		}
		if !pinEditionStale && !editionPhrase(in.Edition).MatchString(section) {
			problems = append(problems, fmt.Sprintf(
				"the \"## %[1]s\" section of editors/vscode/CHANGELOG.md does not say \"edition %[2]s\". "+
					"Name the edition that release speaks.",
				release, in.Edition))
		}
	}
	return problems
}

// changelogSection returns the body of the `## <release>` section: from its
// heading to the next level-two heading. A `### Platforms` subsection belongs
// to the section it sits in, which is why the end test is "## " and not "##".
func changelogSection(changelog, release string) (string, bool) {
	var body []string
	found, inside := false, false
	for _, line := range strings.Split(changelog, "\n") {
		if strings.HasPrefix(line, "## ") {
			if inside {
				break
			}
			fields := strings.Fields(strings.TrimPrefix(line, "## "))
			inside = len(fields) > 0 && fields[0] == release
			found = found || inside
			continue
		}
		if inside {
			body = append(body, line)
		}
	}
	return strings.Join(body, "\n"), found
}

// editionPhrase matches "edition 2026" as prose writes it: at the start of a
// sentence as well as mid-line, and across a hand-wrapped line break.
func editionPhrase(edition string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\bedition\s+` + regexp.QuoteMeta(edition) + `\b`)
}

// parseExtensionRelease reads a plain MAJOR.MINOR.PATCH, the only form the
// publish lane tags. Pre-releases are refused rather than half-understood, so
// a form this gate cannot order is a failure that says so.
func parseExtensionRelease(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		if !isCanonicalVersionComponent(p) {
			return out, false
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func compareExtensionReleases(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// TestExtensionPinsTheLanguageItWasBuiltFrom is the gate over the real tree.
func TestExtensionPinsTheLanguageItWasBuiltFrom(t *testing.T) {
	var manifest extensionLanguageManifest
	loadExtensionJSON(t, checkedInExtensionManifest, &manifest)
	changelog, err := os.ReadFile(checkedInExtensionChangelog)
	if err != nil {
		t.Fatalf("read %s: %v", checkedInExtensionChangelog, err)
	}
	for _, problem := range editorParity(editorParityInput{
		Edition:        parser.Edition,
		GrammarVersion: parser.GrammarVersion,
		EditorRelease:  parser.EditorRelease,
		Manifest:       manifest,
		Changelog:      string(changelog),
	}) {
		t.Error(problem)
	}
}

// TestEditorParityRefusesEachDrift drives the decision with one drift at a
// time. It is what makes a green run above mean something: a checker that
// found nothing wrong with anything would pass the real tree too.
func TestEditorParityRefusesEachDrift(t *testing.T) {
	const (
		edition = "2026"
		grammar = "2026.09-example-grammar-0123abcd"
		release = "0.4.0"
	)
	pinned := func(edition, grammar string) *struct {
		Edition        string `json:"edition"`
		GrammarVersion string `json:"grammarVersion"`
	} {
		return &struct {
			Edition        string `json:"edition"`
			GrammarVersion string `json:"grammarVersion"`
		}{Edition: edition, GrammarVersion: grammar}
	}
	inSync := func() editorParityInput {
		return editorParityInput{
			Edition:        edition,
			GrammarVersion: grammar,
			EditorRelease:  release,
			Manifest:       extensionLanguageManifest{Version: release, MemQL: pinned(edition, grammar)},
			Changelog: "# Changelog\n\n## 0.4.0\n\nSpeaks MemQL edition 2026, grammar `" + grammar + "`.\n\n" +
				"### Platforms\n\nFour.\n\n## 0.3.1\n\nFirst published release.\n",
		}
	}

	if problems := editorParity(inSync()); len(problems) != 0 {
		t.Fatalf("an extension in step with the parser was refused:\n%s", strings.Join(problems, "\n"))
	}

	for _, tc := range []struct {
		name  string
		drift func(*editorParityInput)
		// Every string must appear in the ONE problem reported: the edit it
		// names is the point of the message.
		want []string
	}{
		{
			name:  "the parser's grammar moved and the pin did not",
			drift: func(in *editorParityInput) { in.GrammarVersion = "2026.10-next-grammar-89abcdef" },
			want: []string{
				`set "memql": {"grammarVersion": "2026.10-next-grammar-89abcdef"}`,
				`"## 0.4.0" section, in place of "2026.09-example-grammar-0123abcd"`,
				"make vscode-grammar",
				"If 0.4.0 has already been published",
			},
		},
		{
			name:  "the parser's edition moved and the pin did not",
			drift: func(in *editorParityInput) { in.Edition = "2027" },
			want:  []string{`Set "memql": {"edition": "2027"}`, `say "edition 2027" in the "## 0.4.0" section`},
		},
		{
			name: "the pin moved with the grammar and the changelog did not",
			drift: func(in *editorParityInput) {
				in.GrammarVersion = "2026.10-next-grammar-89abcdef"
				in.Manifest.MemQL = pinned(edition, in.GrammarVersion)
			},
			want: []string{`does not name GrammarVersion "2026.10-next-grammar-89abcdef"`},
		},
		{
			name:  "the manifest carries no pin at all",
			drift: func(in *editorParityInput) { in.Manifest.MemQL = nil },
			want:  []string{`Add "memql": {"edition": "2026", "grammarVersion": "2026.09-example-grammar-0123abcd"}`},
		},
		{
			name:  "EditorRelease is newer than the extension being built",
			drift: func(in *editorParityInput) { in.Manifest.Version = "0.3.1" },
			want:  []string{`Set "version": "0.4.0" in editors/vscode/package.json`},
		},
		{
			name: "the changelog has no section for EditorRelease",
			drift: func(in *editorParityInput) {
				in.Changelog = strings.Replace(in.Changelog, "## 0.4.0", "## 0.3.9", 1)
			},
			want: []string{`has no "## 0.4.0" section`, `names the grammar ("2026.09-example-grammar-0123abcd")`, `"edition 2026"`},
		},
		{
			name: "the section does not name the grammar",
			drift: func(in *editorParityInput) {
				in.Changelog = strings.Replace(in.Changelog, grammar, "2026.08-older-grammar-00000000", 1)
			},
			want: []string{`does not name GrammarVersion "2026.09-example-grammar-0123abcd"`},
		},
		{
			name: "the section does not name the edition",
			drift: func(in *editorParityInput) {
				in.Changelog = strings.Replace(in.Changelog, "edition 2026", "the current edition", 1)
			},
			want: []string{`does not say "edition 2026"`},
		},
		{
			name:  "EditorRelease is not a release",
			drift: func(in *editorParityInput) { in.EditorRelease = "next" },
			want:  []string{`parser.EditorRelease is "next", which is not a MAJOR.MINOR.PATCH release`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := inSync()
			tc.drift(&in)
			problems := editorParity(in)
			// The unparseable EditorRelease also has no changelog section of
			// its own name; every other drift must be reported exactly once.
			if tc.name != "EditorRelease is not a release" && len(problems) != 1 {
				t.Fatalf("want exactly one problem, got %d:\n%s", len(problems), strings.Join(problems, "\n---\n"))
			}
			if len(problems) == 0 {
				t.Fatal("the drift was not reported")
			}
			for _, want := range tc.want {
				if !strings.Contains(problems[0], want) {
					t.Errorf("the problem does not name %q:\n%s", want, problems[0])
				}
			}
		})
	}
}

// TestChangelogSectionStopsAtTheNextRelease pins the section boundary: a
// grammar named only in an OLDER release's section must not satisfy the check
// for the newer one, which is the mistake of editing the wrong heading.
func TestChangelogSectionStopsAtTheNextRelease(t *testing.T) {
	changelog := "## 0.4.0\n\nNew things.\n\n### Platforms\n\nFour.\n\n## 0.3.1\n\ngrammar g-1, edition 2026\n"
	section, found := changelogSection(changelog, "0.4.0")
	if !found {
		t.Fatal("the 0.4.0 section was not found")
	}
	if strings.Contains(section, "g-1") {
		t.Errorf("the 0.4.0 section ran into 0.3.1's:\n%s", section)
	}
	if !strings.Contains(section, "Four.") {
		t.Errorf("a ### subsection was cut off from its own section:\n%s", section)
	}
	if _, found := changelogSection(changelog, "0.4"); found {
		t.Error(`"0.4" matched the "## 0.4.0" heading; a release must match its heading exactly`)
	}
}
