package conformance

// corpus_file_list_test.go -- the frozen edition's machine-readable case list
// (memql#5385 audit). The freeze published test/conformance/2026/manifest.json
// (status "frozen"), but nothing enumerated the FILES the frozen edition
// consists of, so a second implementation wanting to replay exactly the frozen
// set had to walk every expect.json itself, or guess -- and the PR that shipped
// the freeze said as much.
//
// test/conformance/<edition>/files-memql-<language> answers that: one line per
// corpus file a case names (fixture.memql excluded -- it is loaded beside every
// case, never itself a case), path relative to the edition directory, sorted,
// trailing newline, no header. The convention is toml-test's
// files-toml-1.0.0: a plain list a consumer can `read_lines` with no parser of
// its own.
//
// "A file a case names" means exactly what discoverCorpus means by it: this
// test reads the SAME runs every verdict gate reads (corpus_test.go), so the
// list cannot say something the runner does not also enforce (readCorpusDir's
// own "is in the corpus but no case in expect.json names it" check already
// refuses an unnamed .memql file -- this test only writes down the set that
// survives it).
//
// Regenerate with:
//
//	go test github.com/znasllc-io/memql/test/conformance -run TestCorpusFileListMatchesTheLanguageLine -update-file-list

import (
	"flag"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"testing"
)

// updateFileList rewrites the committed file list(s) from discoverCorpus's own
// runs -- the corpus's other -update* flag (-scaffold, corpus_scaffold_test.go)
// never overwrites what exists; this one always does, because the list has no
// content a person hand-edits into it.
var updateFileList = flag.Bool("update-file-list", false,
	"rewrite test/conformance/<edition>/files-memql-<language> from the corpus")

// corpusFileListPath is the committed list's path for one edition/language
// pair, relative to test/conformance (the package's own working directory).
func corpusFileListPath(edition, language string) string {
	return path.Join(edition, fmt.Sprintf("files-memql-%s", language))
}

// corpusFileListFor returns the sorted, deduplicated, edition-relative paths
// of every case file discoverCorpus found for one edition. Fixtures are
// excluded by construction: discoverCorpus never turns a directory's
// fixture.memql into a corpusRun, only the files expect.json's cases name.
//
// Deduplicated because "a file a case names" is not the same count as "a
// case": 877 cases name 715 files, and 69 files carry more than one. Most of
// those (52) are one expression evaluated over several `row` payloads -- nil,
// "", " ", 0, false, unicode, a case fold -- which is one file answering one
// question many times. The rest (17) are a pushdown expression carrying both a
// `lower` case (the SQL) and an `evaluate` case (the post-filter answer) over
// the same source. The list is the SET a second implementation must load, one
// entry per physical file -- not a count of the cases that exercise it.
func corpusFileListFor(runs []*corpusRun, edition string) []string {
	prefix := edition + "/"
	seen := make(map[string]bool, len(runs))
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		if r.edition != edition {
			continue
		}
		rel := strings.TrimPrefix(r.rel, prefix)
		if seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}

// corpusFileListText renders files the way the committed list is written:
// header-free, one path per line, trailing newline.
func corpusFileListText(files []string) string {
	if len(files) == 0 {
		return ""
	}
	return strings.Join(files, "\n") + "\n"
}

// diffSortedLines reports what -update-file-list would add and remove to turn
// old into want, both already sorted. Two pointers over sorted input rather
// than a set: the report is itself in sorted order with no extra pass.
func diffSortedLines(old, want []string) (added, removed []string) {
	i, j := 0, 0
	for i < len(old) && j < len(want) {
		switch {
		case old[i] == want[j]:
			i++
			j++
		case old[i] < want[j]:
			removed = append(removed, old[i])
			i++
		default:
			added = append(added, want[j])
			j++
		}
	}
	removed = append(removed, old[i:]...)
	added = append(added, want[j:]...)
	return added, removed
}

// TestCorpusFileListMatchesTheLanguageLine holds test/conformance/<edition>/
// files-memql-<language> to the corpus every other gate in this package reads:
// added or removed cases must show up in the committed list in the SAME change,
// not be noticed later by a second implementation replaying a set nobody wrote
// down.
func TestCorpusFileListMatchesTheLanguageLine(t *testing.T) {
	runs := discoverCorpus(t)
	if len(runs) == 0 {
		t.Fatal("the corpus holds no cases -- the runner is reading nothing, so a file list generated " +
			"from it would silently claim the frozen edition is empty")
	}

	languageOf := map[string]string{}
	for _, r := range runs {
		if prev, ok := languageOf[r.edition]; ok && prev != r.line.Language {
			t.Fatalf("edition %s has two language lines in play (%q and %q) -- "+
				"every case of one edition shares one manifest.json", r.edition, prev, r.line.Language)
		}
		languageOf[r.edition] = r.line.Language
	}

	for edition, language := range languageOf {
		edition, language := edition, language
		t.Run(edition, func(t *testing.T) {
			files := corpusFileListFor(runs, edition)
			if len(files) == 0 {
				t.Fatalf("edition %s has a manifest but no case files reached it", edition)
			}
			want := corpusFileListText(files)
			listPath := corpusFileListPath(edition, language)

			gotBytes, err := os.ReadFile(listPath)
			if err != nil {
				if !*updateFileList {
					t.Fatalf("%s: %v\nregenerate with:\n\tgo test github.com/znasllc-io/memql/test/conformance "+
						"-run TestCorpusFileListMatchesTheLanguageLine -update-file-list", listPath, err)
				}
				gotBytes = nil
			}
			got := string(gotBytes)
			if got == want {
				return
			}

			if *updateFileList {
				if err := os.WriteFile(listPath, []byte(want), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("rewrote %s (%d files)", listPath, len(files))
				return
			}

			var oldLines []string
			if got != "" {
				oldLines = strings.Split(strings.TrimRight(got, "\n"), "\n")
			}
			added, removed := diffSortedLines(oldLines, files)
			t.Errorf("%s does not match the corpus (%d file(s) added, %d removed).\n"+
				"added:\n  %s\nremoved:\n  %s\nregenerate with:\n\tgo test github.com/znasllc-io/memql/test/conformance "+
				"-run TestCorpusFileListMatchesTheLanguageLine -update-file-list",
				listPath, len(added), len(removed), strings.Join(added, "\n  "), strings.Join(removed, "\n  "))
		})
	}
}
