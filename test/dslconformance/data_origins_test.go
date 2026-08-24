package dslconformance

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/core/dslfs"
	"github.com/znasllc-io/memql/dsl"

	// The connector declarations. Without them this file's
	// registered-connector gate would be measuring an empty registry and
	// would pass on a tree full of typos -- a green that is a statement
	// about the test binary rather than about the corpus.
	_ "github.com/znasllc-io/memql/integrations/shopify"
)

// data_origins_test.go -- the corpus gates for epic memql#4378.
//
// @origin and @mirroredTo turn a concept into one of three states, and
// two of the three change what the engine ACCEPTS: a mirror refuses
// every write that is not its connector's, and an origin appends outbox
// entries on every write. That makes the declarations an authorization
// and a delivery contract rather than metadata, and the failure mode of
// getting one wrong is silence in both directions -- a mirror nobody
// fills reads as an empty catalog, a mirror target nobody drains
// accumulates entries forever.
//
// So the corpus is gated on three things: every concept's state
// DERIVES, every connector it names is one this build serves, and no
// mirror carries a client-reachable mutation.

// loadedConcepts loads the embedded DSL tree into the package registry
// and returns every concept in it.
func loadedConcepts(t *testing.T) []*memoryNodes.Concept {
	t.Helper()
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	all := memoryNodes.List()
	if len(all) == 0 {
		t.Fatal("the concept registry is empty -- this file would pass while measuring nothing")
	}
	return all
}

// Every concept resolves to exactly one of the three states, and the
// declaration it resolves from is coherent.
func TestEveryConceptsDataStateDerives(t *testing.T) {
	all := loadedConcepts(t)

	counts := map[langparser.DataState]int{}
	for _, c := range all {
		if c == nil {
			continue
		}
		decl := c.OriginDecl()
		if err := langparser.ValidateOriginDecl(decl); err != nil {
			t.Errorf("concept %s: %v", c.Name, err)
			continue
		}
		state := decl.State()
		switch state {
		case langparser.DataStateMirror, langparser.DataStateOrigin, langparser.DataStateNative:
			counts[state]++
		default:
			t.Errorf("concept %s derives dataState %q, which is not one of the three states", c.Name, state)
		}
		if got := c.EffectiveOrigin(); strings.TrimSpace(got) == "" {
			t.Errorf("concept %s reports an empty effective origin -- every concept has one, %q by default",
				c.Name, langparser.OriginMemQL)
		}
	}

	t.Logf("\n=== data origins across the tree (epic memql#4378) ===")
	t.Logf("concepts total   %3d", len(all))
	t.Logf("  mirror         %3d", counts[langparser.DataStateMirror])
	t.Logf("  origin         %3d", counts[langparser.DataStateOrigin])
	t.Logf("  native         %3d", counts[langparser.DataStateNative])
}

// Every connector a concept names is one this build serves. The engine
// refuses boot on a miss; this gate says so at PR time, with the whole
// corpus in one message instead of the first failure.
func TestEveryDeclaredConnectorIsRegistered(t *testing.T) {
	all := loadedConcepts(t)

	known := memqlsync.DeclaredNames()
	if len(known) == 0 {
		t.Fatal("no connectors are declared in this test binary -- the gate would pass on a tree full of typos. " +
			"Blank-import the connector packages at the top of this file")
	}

	named := map[string][]string{} // connector -> concepts naming it
	for _, c := range all {
		if c == nil {
			continue
		}
		for _, name := range c.DeclaredConnectors() {
			named[name] = append(named[name], c.Name)
			if !memqlsync.IsDeclared(name) {
				t.Errorf("concept %s names connector %q, which no package in this build declares. "+
					"A mirror nobody fills reads as an empty catalog and a mirror target nobody drains accumulates "+
					"outbox entries forever, so the engine REFUSES BOOT on this. Connectors this build serves: %s",
					c.Name, name, strings.Join(known, ", "))
			}
			if name == langparser.OriginMemQL {
				t.Errorf("concept %s names %q as a connector -- MemQL is not one, and a connector by that name "+
					"would make @origin(%q) ambiguous", c.Name, langparser.OriginMemQL, langparser.OriginMemQL)
			}
		}
	}

	connectors := make([]string, 0, len(named))
	for name := range named {
		connectors = append(connectors, name)
	}
	sort.Strings(connectors)
	t.Logf("\n=== connectors named by the corpus ===")
	t.Logf("declared by this build: %s", strings.Join(known, ", "))
	for _, name := range connectors {
		sort.Strings(named[name])
		t.Logf("  %-12s <- %s", name, strings.Join(named[name], ", "))
	}
}

// A mutation bound to a MIRROR concept must be @serverOnly.
//
// The engine refuses the write regardless, so this is not the gate that
// protects the data -- it is the gate that stops a client being OFFERED
// the write. A client-reachable mutation over a mirror is generated into
// both SDKs as a typed method that can only ever fail, and a UI built on
// it discovers that at runtime, in front of a user, instead of at build
// time.
func TestMirrorConceptsHaveNoClientReachableMutation(t *testing.T) {
	all := loadedConcepts(t)

	mirrors := map[string]string{} // concept -> origin
	for _, c := range all {
		if c != nil && c.IsMirror() {
			mirrors[c.Name] = c.EffectiveOrigin()
		}
	}
	if len(mirrors) == 0 {
		t.Skip("no mirror concepts in the tree yet; this gate has nothing to measure")
	}

	// The corpus is SCANNED rather than loaded, for the reason every
	// gate in this package scans: the loader's registry is reachable
	// only through unexported constructors, and widening component/memql
	// public surface so a conformance test can build a registry is a
	// worse trade than a text pass over 214 files.
	//
	// The binding is matched on the concept's SHORT name, which is what
	// the `mutate <Concept> <name>` signature carries. Two domains could
	// in principle declare the same short name, in which case this gate
	// asks for @serverOnly on a mutation over the wrong one. That is a
	// false FAILURE, which is visible and arguable, rather than a false
	// pass -- the direction a gate about refused writes should err in.
	shortNames := map[string]string{} // short -> "<canonical> (origin <name>)"
	for name, origin := range mirrors {
		parts := strings.Split(name, ":")
		shortNames[parts[len(parts)-1]] = name + " (a mirror of " + origin + ")"
	}

	checked := 0
	for _, m := range scanMutationDeclarations(t) {
		desc, isMirror := shortNames[m.concept]
		if !isMirror {
			continue
		}
		checked++
		if !m.serverOnly {
			t.Errorf("%s: mutation %q writes %s -- the engine refuses every client write to it, so this mutation is "+
				"generated into both SDKs as a typed method that can only fail, and a UI built on it finds out at "+
				"runtime. Mark it @serverOnly (and stamp internal origin in the connector that calls it), or move "+
				"the write behind the connector",
				m.file, m.name, desc)
		}
	}
	// ZERO IS NOW A LEGITIMATE ANSWER, and saying so is not softening the
	// gate (memql#4389).
	//
	// When this landed, every mirror was written by an authored mutation
	// and a scan matching none of them meant the binding match had broken
	// -- a gate measuring nothing while reporting green. Since the Shopify
	// connector, the runtime writes mirror rows through a RAW concept
	// insert from the MirrorWrites a connector returns, so 65 of the 66
	// mirror concepts have no mutation at all. That is the STRONGEST form
	// of what this gate asks for: a mutation nobody can reach because
	// nobody wrote one.
	//
	// The vacuous-pass guard the original `checked == 0` provided is kept
	// by the scan's own floor -- scanMutationDeclarations over the whole
	// corpus must find mutations, or the text pass itself is broken -- so
	// a binding match that silently stopped matching is still caught,
	// without demanding that mirrors be written the older way.
	if total := len(scanMutationDeclarations(t)); total < 100 {
		t.Fatalf("the mutation scan found only %d declarations across the corpus; the text pass is broken, "+
			"and a pass over nothing would report every mirror clean", total)
	}
	t.Logf("%d mutation(s) over %d mirror concept(s), all @serverOnly", checked, len(mirrors))
}

// mutationDecl is one `mutate <Concept> <name>` declaration found in the
// corpus, with whether its preamble carries @serverOnly.
type mutationDecl struct {
	file       string
	concept    string
	name       string
	serverOnly bool
}

var mutateHeaderRe = regexp.MustCompile(`^[ \t]*mutate[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

// scanMutationDeclarations walks the embedded tree and returns every
// mutation declaration with its annotation preamble resolved.
func scanMutationDeclarations(t *testing.T) []mutationDecl {
	t.Helper()
	tree := dsl.Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	var out []mutationDecl
	for _, p := range paths {
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		lines := strings.Split(string(raw), "\n")
		for i, line := range lines {
			m := mutateHeaderRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			out = append(out, mutationDecl{
				file:       p,
				concept:    m[1],
				name:       m[2],
				serverOnly: strings.Contains(strings.Join(preambleFor(lines, i), "\n"), "@serverOnly"),
			})
		}
	}
	if len(out) == 0 {
		t.Fatal("no mutation declarations found in the embedded tree -- the scan is broken")
	}
	return out
}

// The concept doc has to state the contract this file gates, or the
// gate is enforcing a rule only its own source explains.
func TestDataOriginsContractIsDocumented(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "public", "concepts", "data-origins.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("docs/public/concepts/data-origins.md not written yet (issue #4385): %v", err)
	}
	doc := string(raw)
	for _, want := range []string{"@origin", "@mirroredTo", "Mirror", "Origin", "Native"} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/public/concepts/data-origins.md never mentions %q", want)
		}
	}
}

// NOTE on a rule that was drafted here and DELETED rather than kept.
//
// An earlier cut of epic memql#4378 gave the Shopify connector an
// authored, @serverOnly query with no actor term, so that the concept's
// clusterOwner TIER would be injected and then relaxed for the connector
// that owns the mirror. This file gated that shape: "a connector-facing
// read carries no authored actor conjunct".
//
// TestRowAuthzEnforcementLandGate (component/memql) refused the query,
// and it was right to. Its rule is that every authored query over a
// tier-declaring concept states the tier's predicate as a top-level
// conjunct, so that enforcement is a no-op for the corpus rather than a
// silent result-set change -- and a reader of the DSL can tell an
// un-gated read from a deliberate one. A query written specifically to
// be un-gated is exactly what that gate exists to catch.
//
// So the connector's internal sweep is a RAW query string instead,
// guarded by row admission the way the generic concept browse is, and
// the rule that used to live here has no subject left. It is recorded
// rather than silently dropped because the shape is a natural thing to
// reach for again.
