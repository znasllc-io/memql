package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/maketargets"
	"github.com/znasllc-io/memql/core/repowalk"
)

// TestMakeTargetCitationsNameRealTargets is the gate memql#4405 asked for: a
// backtick-quoted `make <target>` anywhere in our Go or Markdown must name a
// target the Makefile has.
//
// WHY IT EARNS A GATE. The drift was not one typo. Error text an operator
// reads at the moment of failure directed them at `secret-set`,
// `secrets-seed` and `dev-refresh` make targets, none of which the Makefile
// has ever had; docs/public/operate/env-vars.md documented a whole
// "concept-stored config" workflow -- init / seed / list / export / one-off
// set -- over a `secrets-*` prefix no target has ever carried, plus a
// ~/.memql/dev-secrets.yaml stash the seeder stopped reading. The tooling
// underneath is real (`scripts/secrets` takes `seed` and `health`), so every
// one of these was a wrapper that was cited but never existed.
//
// It survived because nothing could see it. The citations are spread over Go
// string literals, Go comments and Markdown; no single surface looked wrong;
// and the two tests that mentioned the subject at all asserted
// strings.Contains(err, "make secret-set"), which passed BECAUSE the
// falsehood was there.
//
// THE CONVENTION THIS ENFORCES. A backtick-quoted `make <target>` span is a
// DIRECTIVE -- something a human is being told to run. So a name that is not
// a target has to be spelled some other way: write "a `dev-refresh` make
// target" (as this comment does), never that same name with `make` inside the
// backticks. Prose about a nonexistent command is indistinguishable, to any
// checker, from an instruction to run one, and the tree carries both. This
// gate flagged its own doc comment on first run, which is the convention
// demonstrating itself.
//
// ONE EXEMPTION, and it is the rule's own home. core/maketargets holds the
// extractor plus fixtures that must contain the shape the rule rejects; a
// test proving a dead citation is REPORTED has to contain a dead citation.
// The exemption is a directory, not a pattern, so it cannot spread by
// accident -- and the coverage line below names it.
//
// COVERAGE IS REPORTED, not assumed. A gate that hides what it could not
// examine turns its pass into a claim about the tool rather than the code, so
// this one prints files scanned and citations checked, and FAILS if either
// collapses. A pass over zero citations is the shape a broken extractor has.
func TestMakeTargetCitationsNameRealTargets(t *testing.T) {
	raw, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	real := maketargets.Targets(string(raw))
	if len(real) < 50 {
		t.Fatalf("parsed only %d Makefile targets; the scan below would resolve real "+
			"citations against a set too small to be the Makefile", len(real))
	}

	// The rule's own home. Relative to the repo root, with a trailing
	// separator so it cannot prefix-match a sibling.
	const ruleHome = "core" + string(filepath.Separator) + "maketargets" + string(filepath.Separator)

	type finding struct {
		path   string
		line   int
		target string
		family bool
	}
	var findings []finding
	goFiles, mdFiles, citations, exempt := 0, 0, 0, 0

	err = filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if repowalk.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".go" && ext != ".md" {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		found := maketargets.Citations(string(body))
		if strings.HasPrefix(path, ruleHome) {
			exempt += len(found)
			return nil
		}
		if ext == ".go" {
			goFiles++
		} else {
			mdFiles++
		}
		citations += len(found)
		for _, c := range maketargets.Unknown(string(body), real) {
			findings = append(findings, finding{path: path, line: c.Line, target: c.Target, family: c.Family})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// COVERAGE. Reported unconditionally, and floored so that a pass cannot
	// be a pass over nothing. The floors are far below the measured counts
	// (memql#4405 measured 400+ citations across 700+ files); they exist to
	// catch an extractor or a walk that stopped working, not to pin a number.
	t.Logf("checked %d `make <target>` citations across %d Go files and %d Markdown files "+
		"against %d real Makefile targets (%d citations exempt in %s)",
		citations, goFiles, mdFiles, len(real), exempt, ruleHome)
	if citations < 100 {
		t.Errorf("only %d citations checked; this gate has stopped seeing the tree it is "+
			"supposed to scan (extractor or walk regression), so its pass means nothing", citations)
	}
	if goFiles < 100 || mdFiles < 50 {
		t.Errorf("scanned %d Go and %d Markdown files; the walk is not reaching the repository",
			goFiles, mdFiles)
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].path != findings[j].path {
			return findings[i].path < findings[j].path
		}
		return findings[i].line < findings[j].line
	})
	for _, f := range findings {
		if f.family {
			t.Errorf("%s:%d: `make %s*` names a family of targets, and the Makefile has no "+
				"target starting with %q. Cite a real target, or describe the tool directly "+
				"(e.g. `go run ./scripts/secrets seed`).", f.path, f.line, f.target, f.target)
			continue
		}
		t.Errorf("%s:%d: `make %s` is not a Makefile target. Either add the target, cite a "+
			"real one, or -- if you are writing ABOUT a name that does not exist -- spell it "+
			"without `make` inside the backticks (\"a `%s` make target\"), which is the "+
			"convention this gate enforces (memql#4405).", f.path, f.line, f.target, f.target)
	}
}
