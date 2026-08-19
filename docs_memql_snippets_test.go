package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslimports"
)

// TestDocsMemqlSnippets validates every fenced ```memql code block in
// snippetScope against the real parser (memql#4091, the repo-cleanup-docs-
// update campaign's Task 3), so a DSL example printed in the README cannot
// silently drift from the grammar it claims to demonstrate.
//
// # Why a gate and not just a review pass
//
// The README's flagship examples were WRONG in ways a human proofread never
// caught: `$args`-interpolated object-literal query calls
// (`activeHumanParticipants({...})`) that are not a legal top-level
// declaration at all, a `payload.`-prefixed filter clause retired by epic
// #2292, and a curly-brace logic-step call (`logic autoJoinSI { event:
// event }`) that fails ParseExpression outright -- the parenthesized form
// (`logic autoJoinSI ( event )`) is what every real automation in
// dsl/*/automations.memql uses. None of these announce themselves: a stale
// snippet renders exactly like a working one in GitHub's markdown preview.
//
// # What this validates, and how
//
// Every fenced block whose info string's FIRST TOKEN is `memql` is a
// candidate. Each survivor (see the fence-marker convention below) is
// written to its own t.TempDir() as a single .memql file and loaded through
// the SAME pipeline `cmd/memqllint` runs in single-file mode: dslimports.Load
// (parse + import-graph), then Tree.VerifyReferentialIntegrity,
// Tree.VerifyAllSymbolReferences, and Tree.VerifyPreambleAttachment --
// mirroring cmd/memqllint/main.go's `run()` for a `.memql` file target
// (rootDir = the file's own directory, single file = the whole tree). The
// engine-parity pass (memql.LintUnifiedTree, directory-mode only) is
// deliberately NOT run here: it overlays the linted root on the EMBEDDED
// core dsl/ tree, which a lone example snippet was never written to join,
// so running it would fail every legitimate example for missing neighbors
// it was never meant to have. This is documented rather than silently
// narrower: a snippet passing this gate is proven to PARSE and carry sound
// local references, not proven to mount cleanly as a product bundle.
//
// dslimports is directly importable here (both packages live in this
// module, github.com/znasllc-io/memql) with no need to shell out to
// `go run ./cmd/memqllint`.
//
// # The fence-marker convention (also documented in the campaign's task
// brief, memql#4091's task-3 brief)
//
//	```memql            bare -- validated standalone, must load clean
//	```memql fragment   incomplete by design (e.g. illustrative call syntax
//	                    that is not a legal top-level declaration) -- SKIPPED
//	```memql retired    a deliberate don't-do-this example -- SKIPPED
//
// GitHub's markdown renderer highlights a fenced block by its FIRST token
// only, so a `fragment`/`retired`-marked block still renders with memql
// syntax highlighting -- the marker is invisible to a reader and exists
// purely for this gate.
//
// # Scope
//
// snippetScope is a package-level var so Task 4 (memql#4091's sibling task)
// can widen it to docs/public/** in a one-line change; this task's scope is
// README.md only, per the task-3 brief.
//
// FALSE-POSITIVE ESCAPE HATCH: a snippet that is inherently a fragment (an
// invocation example, a partial construct meant to be read in context) is
// not a bug in the gate -- mark it `fragment`. A snippet that is
// deliberately showing a RETIRED form (a don't-do-this example) is not a bug
// either -- mark it `retired`. Do not special-case a file or block index
// here; the marker convention is the escape hatch.
var snippetScope = []string{"README.md"}

// memqlFenceOpen matches the opening fence line of a ```memql-tagged block.
// The info string is everything after "```memql" on that line, trimmed;
// its first whitespace-separated token (when present) is the marker.
const memqlFenceTag = "```memql"

func TestDocsMemqlSnippets(t *testing.T) {
	var checked, skipped int

	for _, file := range snippetScope {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		blocks := extractMemqlBlocks(string(data))
		if len(blocks) == 0 {
			t.Fatalf("%s: found 0 ```memql fenced blocks -- either the file stopped carrying DSL "+
				"examples, or extractMemqlBlocks stopped recognising the fence syntax. A gate that "+
				"examines nothing passes forever.", file)
		}

		for _, b := range blocks {
			switch b.marker {
			case "fragment", "retired":
				skipped++
				continue
			case "":
				// validated standalone -- fall through
			default:
				t.Errorf("%s: block %d carries unrecognised ```memql marker %q -- the fence-marker "+
					"convention is bare / `fragment` / `retired` only. Fix the marker, or remove it "+
					"to validate the block standalone.", file, b.index, b.marker)
				continue
			}

			checked++
			dir := t.TempDir()
			target := filepath.Join(dir, "snippet.memql")
			if err := os.WriteFile(target, []byte(b.body), 0o644); err != nil {
				t.Fatalf("%s: block %d: write temp snippet: %v", file, b.index, err)
			}

			if diag := validateSnippet(dir); diag != "" {
				t.Errorf("%s: block %d failed to load through the real parser:\n  %s\n"+
					"  (mark the block ```memql fragment if it is deliberately incomplete outside "+
					"its surrounding prose, or ```memql retired if it is a deliberate don't-do-this "+
					"example)", file, b.index, diag)
			}
		}
	}

	if checked == 0 {
		t.Fatalf("every ```memql block across %v was skipped (fragment/retired) -- this gate "+
			"validated nothing. At least one snippet in scope should be a real, standalone example.",
			snippetScope)
	}
	t.Logf("validated %d snippet(s), skipped %d fragment/retired-marked block(s)", checked, skipped)
}

// memqlSnippet is one extracted ```memql-tagged fenced block.
type memqlSnippet struct {
	index  int    // 1-based position among ```memql blocks in the file
	marker string // "" (validate), "fragment", or "retired"
	body   string // the fenced content, excluding the fence lines themselves
}

// extractMemqlBlocks walks content line-by-line (fenced blocks are a line
// construct, not a regex-friendly one once nested backticks and info
// strings are in play) and returns every ```memql-tagged block in document
// order, 1-indexed to match the snippet-probe table's numbering.
func extractMemqlBlocks(content string) []memqlSnippet {
	var blocks []memqlSnippet
	lines := strings.Split(content, "\n")

	inBlock := false
	var marker string
	var body []string

	for _, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		if !inBlock {
			if !strings.HasPrefix(trimmed, memqlFenceTag) {
				continue
			}
			// Only a fence whose info string's first token is exactly
			// "memql" counts -- "```memqlfoo" (no separating space) is a
			// different (unrecognised) language tag, not a marked memql
			// block, and must not be swept in here.
			rest := trimmed[len(memqlFenceTag):]
			if rest != "" && !strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "\t") {
				continue
			}
			marker = strings.TrimSpace(rest)
			inBlock = true
			body = nil
			continue
		}
		// Inside a block: a line that is a closing fence ends it. A closing
		// fence is a line whose trimmed content is exactly a run of three or
		// more backticks (no info string).
		fenceLine := strings.TrimSpace(trimmed)
		if strings.HasPrefix(fenceLine, "```") && strings.Trim(fenceLine, "`") == "" {
			blocks = append(blocks, memqlSnippet{
				index:  len(blocks) + 1,
				marker: marker,
				body:   strings.Join(body, "\n") + "\n",
			})
			inBlock = false
			continue
		}
		body = append(body, trimmed)
	}
	return blocks
}

// validateSnippet loads dir (which holds exactly one .memql file) through
// the same pipeline cmd/memqllint runs for a single-file target, and
// returns the first diagnostic as a string ("" when clean). Mirrors
// cmd/memqllint/main.go's run() for the `info.IsDir()` == false /
// `.memql` case, minus the engine-parity overlay pass (directory-mode
// only -- see the package doc comment on TestDocsMemqlSnippets for why that
// pass does not apply to a lone snippet).
func validateSnippet(dir string) string {
	root := os.DirFS(dir)
	tree, loadErr := dslimports.Load(root)
	if loadErr != nil {
		var le *dslimports.LoadError
		if errors.As(loadErr, &le) && len(le.Diagnostics) > 0 {
			return le.Diagnostics[0].Error()
		}
		return loadErr.Error()
	}
	if tree == nil {
		return "load produced no tree and no error (unexpected)"
	}
	for _, e := range tree.VerifyReferentialIntegrity() {
		return e.Error()
	}
	for _, e := range tree.VerifyAllSymbolReferences() {
		return e.Error()
	}
	for _, e := range tree.VerifyPreambleAttachment() {
		return e.Error()
	}
	return ""
}
