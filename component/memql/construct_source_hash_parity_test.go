package memql

// construct_source_hash_parity_test.go is THE gate memql#3758 exists for: the
// engine and the language server must compute the same construct source hash
// for every construct in the tree.
//
// # Why a corpus gate rather than more unit tests
//
// The hash function is already shared -- one implementation in
// component/memql/sense/sourcehash.go, imported by both sides -- and it already
// has unit coverage for what must and must not change it
// (sense/sourcehash_test.go). So arithmetic is not where the two can disagree;
// both call the same SHA-256 over the same normalizer.
//
// Where they CAN disagree, and the only place, is over which bytes of the file
// are "this construct's source":
//
//   - the ENGINE cuts a construct out with a per-keyword header regexp plus a
//     brace match (keywordHeaderRegexp -> parser.ExtractDeclarationSlices),
//     because it is reading finished files off disk;
//   - the LANGUAGE SERVER locates one by walking the token stream
//     (sense.ConstructHashes -> scanTopLevelConstructs), because it has to keep
//     working on a half-typed buffer in which the regexp's `{` has not been
//     typed yet.
//
// Two mechanisms, one question. A disagreement of a single line makes the
// hashes differ for a construct nobody has touched, and the symptom is not
// "one construct is wrong" -- it is EVERY construct reading as drifted at once,
// which looks like a broken cluster and gets debugged in the wrong place. That
// is why the issue asks for this gate before anything consumes the hash, and
// why a failure here shouts that it is a PARITY failure rather than drift.
//
// It earned its keep twice on the first run:
//
//   - 932 constructs carry a `///` doc comment above their annotation block.
//     Comments are not tokens, so the token scan started the declaration at the
//     first `@` and dropped the doc comment -- which the hash deliberately
//     KEEPS, being the description the catalog serves. Fixed by giving both
//     sides one implementation of "where does a declaration's text begin"
//     (parser.PreambleStartOf).
//   - 10 automations are authored in the terse single-line form, which has no
//     brace for the engine's header regexp to find, so the engine's source
//     index held no entry and stamped an EMPTY hash while the language server
//     computed a real one. Fixed in buildConstructSourceIndex.

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/baseloader"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// parityConstruct is one construct as one side sees it: where it was found, and
// the two things a failure has to be able to show -- the hash, and the text it
// was computed over.
type parityConstruct struct {
	path   string
	source string
	hash   string
}

// TestConstructSourceHashParityAcrossTheDSLCorpus runs both implementations
// over every construct in the mounted DSL tree and asserts they agree.
func TestConstructSourceHashParityAcrossTheDSLCorpus(t *testing.T) {
	engineSide, lspSide := bothSides(t)

	var mismatched []string
	compared := 0
	for key, engineAt := range engineSide {
		lspAt, found := lspSide[key]
		if !found {
			continue // the coverage gate below owns this failure
		}
		compared++
		if engineAt.hash == lspAt.hash {
			continue
		}
		mismatched = append(mismatched, describeHashDivergence(key, engineAt, lspAt))
	}

	if compared == 0 {
		t.Fatal("no construct was located by BOTH sides: the two keyings have diverged, " +
			"so this gate is comparing nothing and would pass on any hash whatsoever")
	}
	if len(mismatched) > 0 {
		sort.Strings(mismatched)
		t.Errorf("%s\n%d of %d constructs hashed differently on the two sides.\n\n%s",
			parityBanner, len(mismatched), compared, strings.Join(capReport(mismatched, 8), "\n"))
	}
	t.Logf("compared %d constructs located by both sides (engine located %d, language server %d)",
		compared, len(engineSide), len(lspSide))
}

// TestConstructSourceHashParityCoversTheSameConstructs is the other half of
// parity, and the half that is easy to forget: two sides that agree perfectly
// about every construct they BOTH find can still disagree about which
// constructs exist.
//
// That asymmetry is not benign. A construct the language server locates and the
// engine's source index does not gets an EMPTY hash from the catalog, so it
// compares unequal to the real one the editor computed and reads as drifted --
// permanently, since no edit can make an empty hash match. A construct only the
// engine finds is the mirror: the editor offers no state at all for a construct
// the cluster knows about.
func TestConstructSourceHashParityCoversTheSameConstructs(t *testing.T) {
	engineSide, lspSide := bothSides(t)

	var engineOnly, lspOnly []string
	for key, at := range engineSide {
		if _, found := lspSide[key]; !found {
			engineOnly = append(engineOnly, fmt.Sprintf("  %s  (%s)", describeKey(key), at.path))
		}
	}
	for key, at := range lspSide {
		if _, found := engineSide[key]; !found {
			lspOnly = append(lspOnly, fmt.Sprintf("  %s  (%s)", describeKey(key), at.path))
		}
	}

	if len(engineOnly) > 0 {
		sort.Strings(engineOnly)
		t.Errorf("%s\n%d constructs the ENGINE slices but the language server never locates.\n"+
			"The editor shows no training state at all for these.\n\n%s",
			parityBanner, len(engineOnly), strings.Join(capReport(engineOnly, 15), "\n"))
	}
	if len(lspOnly) > 0 {
		sort.Strings(lspOnly)
		t.Errorf("%s\n%d constructs the LANGUAGE SERVER locates but the engine's source index does not.\n"+
			"The catalog stamps an EMPTY hash for these, so they read as drifted forever -- no edit\n"+
			"can make an empty hash match a real one.\n\n%s",
			parityBanner, len(lspOnly), strings.Join(capReport(lspOnly, 15), "\n"))
	}
}

// TestConstructSourceHashParityKnowsEveryCatalogedKind pins the kind sets
// themselves. The two gates above compare (kind, name) pairs, so a kind ONE
// side has never heard of produces no pairs and therefore no failure -- the
// gate would go quietly blind to a whole construct kind instead of reporting
// it. This asserts every kind the engine catalogs is a keyword the language
// server's scanner recognises.
func TestConstructSourceHashParityKnowsEveryCatalogedKind(t *testing.T) {
	for kind, keyword := range constructKeyword {
		// The one-identifier signature parses for every kind, and locating a
		// declaration is all the scanner is being asked to do here -- the
		// binder form of `query <Concept> <name>` is exercised by the corpus
		// gates above, on every real two-identifier construct in the tree.
		found := sense.ConstructHashes(keyword + " probeConstructName {\n}\n")
		if len(found) != 1 || found[0].Kind != keyword {
			t.Errorf("the language server's scanner does not recognise %q (catalog kind %q): "+
				"every construct of that kind is invisible to drift detection, and the corpus "+
				"parity gates would silently compare zero of them",
				keyword, kind)
		}
	}
}

// parityBanner heads every failure in this file. The issue asks for a parity
// failure to be its own distinct message and never to read as "everything is
// drifted", because from the outside the two look identical and only one of
// them is a problem with the cluster.
const parityBanner = "" +
	"CONSTRUCT SOURCE HASH PARITY FAILURE (memql#3758) -- this is NOT drift.\n" +
	"The engine and the language server disagree about a construct nobody has edited.\n" +
	"Left unfixed this makes constructs read as `drifted` in the editor, which looks like a\n" +
	"broken cluster and gets debugged in the wrong place. Fix the SLICING, not the hash:\n" +
	"  engine ........  buildConstructSourceIndex -> parser.ExtractDeclarationSlices\n" +
	"  lang server ...  sense.ConstructHashes -> scanTopLevelConstructs\n"

// bothSides reads the tree once and returns each side's answer, keyed
// identically. It fails the test rather than returning empty maps: a gate that
// walked zero files, or whose two keyings share no key, passes vacuously, and a
// vacuous pass on this particular contract is worse than no gate at all.
func bothSides(t *testing.T) (engineSide, lspSide map[string]parityConstruct) {
	t.Helper()
	files := baseloader.ReadAll(nil)
	if len(files) == 0 {
		t.Fatal("the DSL tree walked to zero files: this gate would pass vacuously")
	}
	engineSide = engineConstructHashes()
	if len(engineSide) == 0 {
		t.Fatal("the engine's source index held zero constructs: this gate would pass vacuously")
	}
	lspSide = lspConstructHashes(files)
	if len(lspSide) == 0 {
		t.Fatal("the language server located zero constructs: this gate would pass vacuously")
	}
	return engineSide, lspSide
}

// engineConstructHashes is the ENGINE's answer: the source index the construct
// catalog builds, hashed exactly as ConstructCatalog hashes it.
//
// It calls buildConstructSourceIndex rather than re-deriving the slices from
// keywordHeaderRegexp itself. A gate that re-implements the thing it guards
// proves nothing about the code that ships -- and this one would be especially
// hollow, since the divergence class it exists to catch is "two
// implementations of one slice".
func engineConstructHashes() map[string]parityConstruct {
	out := map[string]parityConstruct{}
	for key, src := range buildConstructSourceIndex().byKindName {
		out[key] = parityConstruct{
			path:   src.path,
			source: src.source,
			hash:   sense.ConstructSourceHash(src.source),
		}
	}
	return out
}

// lspConstructHashes is the LANGUAGE SERVER's answer over the same files,
// re-keyed onto the engine's (kind, name) keying so the two are comparable.
//
// First-wins on a duplicate name mirrors buildConstructSourceIndex, over the
// same file order, so a name declared twice compares the same declaration on
// both sides rather than two different ones -- which would show up as a hash
// mismatch that is really a keying artifact.
func lspConstructHashes(files []baseloader.RawFile) map[string]parityConstruct {
	kindOfKeyword := map[string]string{}
	for kind, keyword := range constructKeyword {
		kindOfKeyword[keyword] = kind
	}

	out := map[string]parityConstruct{}
	for _, f := range files {
		domain := treeDomainOf(f.Path)
		runes := []rune(f.Content)
		for _, c := range sense.ConstructHashes(f.Content) {
			kind, known := kindOfKeyword[c.Kind]
			if !known {
				// A keyword the engine does not catalog (`use`, or a kind a
				// future grammar epic adds). Not this gate's business: there is
				// no catalog entry for it to be compared against.
				continue
			}
			key := kind + "\x00" + c.Name
			if kind == ConstructKindConcept {
				key = kind + "\x00" + domain + "/" + c.Name
			}
			if _, exists := out[key]; exists {
				continue
			}
			source := ""
			if c.Start >= 0 && c.End <= len(runes) && c.Start < c.End {
				source = string(runes[c.Start:c.End])
			}
			out[key] = parityConstruct{path: f.Path, source: source, hash: c.SourceHash}
		}
	}
	return out
}

// describeHashDivergence says WHICH construct diverged and WHAT differs about
// it.
//
// The normalized forms are what actually got hashed, so the first byte at which
// they part IS the answer -- and printing the raw sources instead would bury it
// under the comments and indentation the normalizer had already removed. This
// is what sense.NormalizeConstructSource is exported for.
func describeHashDivergence(key string, engineAt, lspAt parityConstruct) string {
	engineNorm := sense.NormalizeConstructSource(engineAt.source)
	lspNorm := sense.NormalizeConstructSource(lspAt.source)

	var b strings.Builder
	fmt.Fprintf(&b, "  %s\n    file ......... %s\n", describeKey(key), engineAt.path)
	fmt.Fprintf(&b, "    engine hash .. %s\n    lang server .. %s\n", shortHash(engineAt.hash), shortHash(lspAt.hash))
	switch i := firstDifference(engineNorm, lspNorm); {
	case i >= 0:
		fmt.Fprintf(&b, "    the two normalized forms first differ at byte %d:\n", i)
		fmt.Fprintf(&b, "      engine ...... %s\n", excerpt(engineNorm, i))
		fmt.Fprintf(&b, "      lang server . %s\n", excerpt(lspNorm, i))
	default:
		// Identical normalized forms with different hashes would mean the hash
		// is not a function of its input. Worth saying out loud rather than
		// printing an empty diff and leaving the reader to infer it.
		b.WriteString("    the normalized forms are IDENTICAL, so the divergence is in the hash function itself\n")
	}
	return b.String()
}

// describeKey turns an index key back into something a person can read.
func describeKey(key string) string {
	kind, name, found := strings.Cut(key, "\x00")
	if !found {
		return key
	}
	return kind + " " + name
}

// firstDifference returns the first byte offset at which a and b differ, or -1
// when they are equal. A prefix that runs out counts as differing at its end,
// which is the common shape here: one side's slice starts a line earlier than
// the other's.
func firstDifference(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}

// excerpt renders a short quoted window around an offset.
func excerpt(s string, i int) string {
	start := i - 30
	if start < 0 {
		start = 0
	}
	end := i + 60
	if end > len(s) {
		end = len(s)
	}
	if start >= end {
		return `""`
	}
	return fmt.Sprintf("%q", s[start:end])
}

// shortHash truncates a hash for a failure message, keeping enough to tell two
// apart. An EMPTY hash is spelled out rather than printed as "": it is exactly
// the value a construct the engine never sliced reports, it is the single most
// likely parity failure, and as an empty pair of quotes it is easy to miss.
func shortHash(hash string) string {
	if hash == "" {
		return "<EMPTY -- this side never sliced the construct at all>"
	}
	if len(hash) <= 16 {
		return hash
	}
	return hash[:16] + "..."
}

// capReport trims a report to at most n entries, appending a count of what was
// dropped. A parity break is usually total rather than isolated, so an uncapped
// report is thousands of lines all saying the same thing -- and the count at
// the end is the part that actually tells the reader which kind of break it is.
func capReport(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return append(append([]string{}, items[:n]...),
		fmt.Sprintf("  ... and %d more (a parity break is usually total, not isolated)", len(items)-n))
}
