package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
)

// The docs bundle strips HTML comments (memql#5721).
//
// The comments in docs/public are gate markers -- `<!-- corpus: -->` beside a
// tested example, `<!-- proving: -->` beside a claim, `<!-- retired-vocabulary-ok:
// -->` beside a legitimate use of a retired word, `<!-- BEGIN GENERATED -->`
// around generated text. The gates read them from the SOURCE tree. The site
// renders the BUNDLE with react-markdown and no raw-HTML plugin, which prints
// a comment as literal text, so every marker that reached the bundle showed up
// mid-paragraph on memql.io. scripts/docs/_bundle.py now strips them outside
// fenced code and code spans, where a comment is content.
//
// Both tests run the real bundler; they skip only where python3 is absent,
// the same allowance scripts/deploy's python-backed tests make.

func runDocsBundler(t *testing.T, publicDir string) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	out := t.TempDir()
	cmd := exec.Command("python3", filepath.Join("scripts", "docs", "_bundle.py"))
	cmd.Env = append(os.Environ(),
		"PUBLIC_DIR="+publicDir,
		"OUT="+out,
		"VERSION=0.0.0-test",
		"ENGINE_VERSION=0.0.0-test",
	)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("scripts/docs/_bundle.py: %v\n%s", err, b)
	}
	return out
}

func TestDocsBundleStripsHTMLCommentsOutsideCode(t *testing.T) {
	public := t.TempDir()
	source := strings.Join([]string{
		"---",
		"title: Fixture",
		"audience: public",
		"area: operate",
		"---",
		"",
		"Intro paragraph.",
		"<!-- corpus: 2026/examples/fixture.memql -->",
		"```memql",
		"<!-- inside a fence, this is content -->",
		"```",
		"",
		"The label `portal` is reserved. <!-- retired-vocabulary-ok: a reason -->",
		"A span `<!-- kept -->` stays, and <!-- gone --> goes.",
		"| a | b <!-- proving-pending: metric=x --> | c |",
		"Before.",
		"<!-- BEGIN GENERATED",
		"spanning lines",
		"-->",
		"After.",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(public, "fixture.md"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	out := runDocsBundler(t, public)
	got, err := os.ReadFile(filepath.Join(out, "fixture.md"))
	if err != nil {
		t.Fatalf("the fixture was not bundled: %v", err)
	}

	want := strings.Join([]string{
		"---",
		"title: Fixture",
		"audience: public",
		"area: operate",
		"---",
		"",
		"Intro paragraph.",
		"",
		"```memql",
		"<!-- inside a fence, this is content -->",
		"```",
		"",
		"The label `portal` is reserved.",
		"A span `<!-- kept -->` stays, and  goes.",
		"| a | b  | c |",
		"Before.",
		"",
		"After.",
		"",
	}, "\n")
	if string(got) != want {
		t.Errorf("bundled markdown differs from the expected strip.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// bundleFence matches a line that opens or closes a fenced code block. A marker
// inside a fence is an example of one and is bundled as written.
var bundleFence = regexp.MustCompile("^[ \t]*(```|~~~)")

// TestDocsBundleCarriesNoGateMarkers bundles the real docs/public and asserts
// that no gate marker survives into it outside a fenced code block -- the
// property the site depends on.
func TestDocsBundleCarriesNoGateMarkers(t *testing.T) {
	public, err := filepath.Abs(filepath.Join("docs", "public"))
	if err != nil {
		t.Fatal(err)
	}
	out := runDocsBundler(t, public)

	markers := []string{vocabOKMarker, "<!-- corpus:", "<!-- proving", "<!-- BEGIN GENERATED", "<!-- deprecation-window"}
	var bundled int
	walkErr := filepath.WalkDir(out, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && repowalk.SkipDir(d.Name()) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		bundled++
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		inFence := false
		for i, line := range strings.Split(string(data), "\n") {
			if bundleFence.MatchString(line) {
				inFence = !inFence
				continue
			}
			if inFence {
				continue
			}
			for _, m := range markers {
				if strings.Contains(line, m) {
					rel, _ := filepath.Rel(out, path)
					t.Errorf("%s:%d: the gate marker %q reached the docs bundle, where the site prints it as text:\n  %s",
						rel, i+1, m, strings.TrimSpace(line))
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	if bundled == 0 {
		t.Fatal("the bundler selected no public docs, so this test examined nothing")
	}
}
