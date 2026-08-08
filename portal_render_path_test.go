package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// renderPathPrefixes are the portal directories that make up the CONCEPT
// BROWSE PATH -- the code that turns an arbitrary concept's registry entry,
// schema and rows into pixels (znasllc-io/memql#3316).
//
// A directory prefix rather than a list of files, deliberately: a closed file
// list is a gate that goes quiet the moment someone adds a file to it, and the
// file they add is exactly where the first concept-specific branch would land.
var renderPathPrefixes = []string{
	// The browse-path logic: registry search, the keyset walk, the CDC merge
	// policy, schema description, URL building.
	"clients/portal/src/concepts/",
	// The presentational components: the row list, the row detail, the shared
	// status messages they render through.
	"clients/portal/src/components/",
	// The view-kit adapter: the VNode -> React walk and the row projection it
	// is fed.
	"clients/portal/src/viewkit/",
}

// renderPathPageFiles are the browse-path PAGES. Named individually because
// clients/portal/src/pages/ also holds sign-in and OAuth-callback screens,
// which have nothing to do with concept rendering and no reason to be policed.
var renderPathPageFiles = []string{
	"clients/portal/src/pages/ConceptPage.tsx",
	"clients/portal/src/pages/ConceptRowsPane.tsx",
	"clients/portal/src/pages/ConceptSchemaPane.tsx",
	"clients/portal/src/pages/ConceptsPage.tsx",
	"clients/portal/src/pages/conceptContext.ts",
}

// conceptIDLiteral matches a memQL concept id: a version segment, a domain and
// an entity, colon-separated (the `{version}:{domain}:{entity}` grammar in
// docs/public/concepts/identifiers.md). A row id -- the same prefix plus a
// short id -- matches too, which is intended: hard-coding one row is as much a
// violation as hard-coding one concept.
var conceptIDLiteral = regexp.MustCompile(`\bv[0-9]+:[A-Za-z][A-Za-z0-9_]*:[A-Za-z][A-Za-z0-9_]*`)

// TestPortalRenderPathIsConceptAgnostic is the mechanical half of memql#3316's
// one non-negotiable rule: NO CONCEPT-SPECIFIC RENDERING CODE, EVER.
//
// # What the rule buys
//
// A row projects through whatever `@displayCard` slots its concept declares,
// falling back to view-kit's documented inference (sdk/ts-viewkit/src/
// displayCard.ts, docs/public/concepts/display-cards.md) when it declares
// none; a row's detail projects through a recursive walk that preserves the
// wire's nesting. Neither renderer knows the name of any concept. That is what
// makes a concept declared today work in the portal today -- no renderer
// change, no release -- and it is the base the view system (#3317 / #3318 /
// #3319) extends rather than replaces.
//
// # Why a text scan rather than a behavioural test
//
// There IS a behavioural test: clients/portal/test/conceptAgnostic.test.tsx
// renders several structurally different concepts through the same components
// and asserts each is projected through its own card. That test proves the
// renderers work generically; it CANNOT prove that no special case exists,
// because a special case for a concept the test does not happen to use passes
// it silently. The scan is the complement -- it proves the absence, which is
// the property the rule is actually about.
//
// # Why it lives in the root package and not in vitest
//
// The lesson of scripts/dev/vscode_lane_scope_test.go, restated by
// clients_allowlist_test.go: a guard placed where the thing it polices could
// disable it goes silent at exactly the wrong moment. A vitest guard lives
// inside clients/portal, so the same change that adds
// `if (conceptId === "...")` can delete the test that forbids it, and the
// portal lane goes green. This guard is a `.go` file outside that tree:
// weakening it edits Go (the `go` CI bucket, which runs the root package
// uncached), and editing the render path without touching it still runs here
// because `clients/**` is a `gates` entry in .github/workflows/ci.yml -- the
// bucket whose stated purpose is "a non-Go file that a Go gate reads"
// (memql#2972). Both dodges have to run this test.
//
// # If this fails
//
// The fix is never to add the file to an exemption list -- there is none, on
// purpose. It is to express the behaviour through the concept's DECLARATION:
// a `@displayCard(...)` slot, a field annotation the schema view already
// surfaces generically, or a view definition (#3319). If a genuinely generic
// capability needs a new hint, it belongs on the wire as a per-concept hint
// every concept can carry, not as a branch in a renderer.
func TestPortalRenderPathIsConceptAgnostic(t *testing.T) {
	files := renderPathFiles(t)

	// Anti-vacuous floor. A guard that enumerated nothing would report that
	// every file is clean, having read none -- which is how a renamed tree
	// turns a gate green forever.
	if len(files) < 8 {
		t.Fatalf("the concept browse path scanned only %d files (%v), which is fewer "+
			"than the portal carries. If clients/portal moved or was restructured, "+
			"retarget renderPathPrefixes / renderPathPageFiles rather than letting "+
			"this guard pass vacuously (memql#3316).", len(files), files)
	}

	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(".", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			match := conceptIDLiteral.FindString(line)
			if match == "" {
				continue
			}
			t.Errorf("%s:%d names the concept id %q.\n\n"+
				"THE RULE (memql#3316): there is NO concept-specific rendering code on "+
				"the portal's browse path, ever. A row projects through whatever "+
				"@displayCard slots its concept declares, or through view-kit's "+
				"documented inference when it declares none "+
				"(docs/public/concepts/display-cards.md); a row's detail projects "+
				"through a recursive walk of the wire shape. That is what makes a "+
				"concept declared today render today, with no renderer change -- and "+
				"it is the base #3317 / #3318 / #3319 build on.\n\n"+
				"THE SCAN COVERS COMMENTS TOO, and that is deliberate: an id in a "+
				"comment is one copy-paste away from being an id in a branch, and a "+
				"rule with no exceptions needs no adjudication. Write the id as "+
				"<version>:<domain>:<entity> in prose, and put real ids in test "+
				"fixtures (clients/portal/test/**), which this guard does not police.\n\n"+
				"IF YOU NEED PER-CONCEPT BEHAVIOUR: declare it on the CONCEPT (a "+
				"@displayCard slot, a field annotation the schema view already renders "+
				"generically, or a view definition), not as a branch here.\n\n"+
				"line: %s", rel, i+1, match, strings.TrimSpace(line))
			// One report per file is enough to act on; a concept id repeated
			// forty times in one file would otherwise bury every other failure.
			break
		}
	}
}

// renderPathFiles returns the TypeScript files on the browse path.
//
// It walks the FILESYSTEM rather than `git ls-files`, which is where this
// guard differs from its siblings (clients_allowlist_test.go,
// product_neutrality_test.go) -- and the difference is deliberate. Those ask
// "what does this repo CONTAIN", a question only tracked files can answer. This
// one asks "what does the code the portal builds DO", and vite bundles a file
// the moment something imports it, tracked or not. A working tree that
// typechecks and ships a concept-specific branch out of an unstaged file is
// exactly the state this must fail in, not wave through.
func renderPathFiles(t *testing.T) []string {
	t.Helper()

	wantFile := make(map[string]bool, len(renderPathPageFiles))
	for _, name := range renderPathPageFiles {
		wantFile[name] = false
	}

	seen := make(map[string]struct{})
	var files []string
	add := func(rel string) {
		if _, dup := seen[rel]; dup {
			return
		}
		seen[rel] = struct{}{}
		files = append(files, rel)
	}

	for _, prefix := range renderPathPrefixes {
		dir := filepath.FromSlash(strings.TrimSuffix(prefix, "/"))
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".ts") && !strings.HasSuffix(path, ".tsx") {
				return nil
			}
			add(filepath.ToSlash(path))
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v. If the portal tree moved, retarget renderPathPrefixes "+
				"rather than deleting the row (memql#3316).", prefix, err)
		}
	}

	// A named page that no longer exists is a stale row: it silently shrinks
	// the policed surface, and whoever renamed it gets no signal.
	var missing []string
	for name := range wantFile {
		info, err := os.Stat(filepath.FromSlash(name))
		if err != nil || info.IsDir() {
			missing = append(missing, name)
			continue
		}
		wantFile[name] = true
		add(name)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("renderPathPageFiles names %v, which do not exist. Retarget the rows "+
			"(or drop them if the page is gone) -- a stale row quietly removes a page "+
			"from the concept-agnostic guard (memql#3316).", missing)
	}

	sort.Strings(files)
	return files
}
