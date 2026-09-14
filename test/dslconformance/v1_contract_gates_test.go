package dslconformance

// v1_contract_gates_test.go -- the load-time contract gates (dslgate) over the
// migrated corpus (epic memql#5363, task memql#5368).
//
// The engine refuses a boot on these gates, so what they say about the tree
// the codemod produces is what a node will say the day it boots that tree.
// Three properties are asserted, each on both corpora:
//
//  1. the migrated corpus is as clean as the embedded one;
//  2. every construct classifies into the SAME per-row authz bucket in both
//     editions -- a construct whose migrated form reads `other` where its
//     legacy form read `owned` is a gate that changed its mind about who may
//     read which rows because a spelling moved;
//  3. the gate still CATCHES on the real corpus: every caller check in both
//     corpora is replaced by a caller-supplied argument, and each construct
//     that selected its rows by ownership must now land in the hard-failing
//     bucket, identically in both editions. The corpus is clean, so without
//     this the parity in (2) could hold between two blind gates.

import (
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/dslclause"
	"github.com/znasllc-io/memql/component/memql/dslgate"
)

func serverOnlyOptions(t *testing.T) dslgate.Options {
	t.Helper()
	serverOnly := serverOnlyConstructs(t)
	return dslgate.Options{
		ServerOnly: func(file, name string) bool {
			return serverOnly[serverOnlyKey{Path: file, Name: name}]
		},
	}
}

// classifyCorpus returns every row-accessing construct's bucket, keyed
// "<file> <kind> <name>".
func classifyCorpus(c corpus, opts dslgate.Options) map[string]dslgate.Bucket {
	out := map[string]dslgate.Bucket{}
	for _, p := range c.paths {
		for _, cl := range dslgate.ClassifySource(p, c.files[p], opts) {
			out[p+" "+cl.Kind+" "+cl.Name] = cl.Bucket
		}
	}
	return out
}

func TestContractGatesPassTheMigratedCorpus(t *testing.T) {
	opts := serverOnlyOptions(t)
	bothCorpora(t, func(t *testing.T, c corpus) {
		for _, v := range dslgate.ScanFiles(c.sources(), opts) {
			t.Errorf("%s", v)
		}
	})
}

func TestAuthzClassificationIsEditionIndependent(t *testing.T) {
	opts := serverOnlyOptions(t)
	legacy := classifyCorpus(embeddedCorpus(t), opts)
	v1 := classifyCorpus(migratedCorpus(t), opts)

	counts := map[dslgate.Bucket]int{}
	for key, want := range legacy {
		got, ok := v1[key]
		if !ok {
			t.Errorf("%s: classified %s in the embedded tree and not found in the migrated one -- the construct walk has stopped seeing it", key, want)
			continue
		}
		if got != want {
			t.Errorf("%s: classifies %s in the embedded tree and %s once migrated", key, want, got)
		}
		counts[got]++
	}
	for key := range v1 {
		if _, ok := legacy[key]; !ok {
			t.Errorf("%s: found only in the migrated corpus", key)
		}
	}
	// Floors: measured when this landed at 251 owned and 228 admin over 1003
	// row-accessing constructs. A walk that stopped finding constructs, or a
	// classifier that stopped reading v1 filters, drops through these.
	if counts[dslgate.BucketOwned] < 200 || counts[dslgate.BucketAdmin] < 150 || len(legacy) < 800 {
		t.Errorf("classification floors: %d constructs, %d owned, %d admin -- the classifier is no longer reading what it used to", len(legacy), counts[dslgate.BucketOwned], counts[dslgate.BucketAdmin])
	}
	t.Logf("%d constructs classified identically in both editions: %v", len(legacy), counts)
}

// injectCallerArg replaces every caller check a filter can make with a
// caller-SUPPLIED value, the substitution that turns an owner-scoped read into
// the one the user-scope gate exists to refuse.
func injectCallerArg(c corpus) corpus {
	files := make(map[string]string, len(c.files))
	for p, s := range c.files {
		files[p] = strings.ReplaceAll(s, "actor.userId", "args.injectedCallerId")
	}
	return newCorpus(c.name+"+injected", files)
}

// injectIntoFilters appends suffix to the END of every filter clause in the
// corpus -- after its last continuation line, before any trailing comment --
// so the injected term composes with the whole clause in both editions: the
// codemod wraps a long v1 clause across lines, and a term appended to its
// first line would bind to one conjunct rather than to the clause. It returns
// the new corpus and how many clauses were injected into.
func injectIntoFilters(c corpus, suffix string) (corpus, int) {
	files := make(map[string]string, len(c.files))
	injected := 0
	for p, s := range c.files {
		lines := strings.Split(s, "\n")
		code := strings.Split(blankComments(s), "\n")
		for i := 0; i < len(lines); i++ {
			trim := strings.TrimSpace(code[i])
			if !strings.HasPrefix(trim, "filter ") && !strings.HasPrefix(trim, "filter\t") {
				continue
			}
			last := dslclause.ClauseExtent(code, i)
			end := len(strings.TrimRight(code[last], " \t"))
			lines[last] = lines[last][:end] + suffix + lines[last][end:]
			injected++
			i = last
		}
		files[p] = strings.Join(lines, "\n")
	}
	return newCorpus(c.name+"+injected", files), injected
}

// TestContractGatesCatchOnTheRealCorpus is the CATCH half of every
// filter-reading contract gate, on the real corpus in both editions: a
// violation is written into every filter clause the corpus has, and each
// gate must report every one of them. The corpus is clean, so without this a
// gate that went blind on the migrated form would read as a gate with nothing
// to report.
func TestContractGatesCatchOnTheRealCorpus(t *testing.T) {
	opts := serverOnlyOptions(t)
	for _, tc := range []struct {
		gate   dslgate.Gate
		suffix string
	}{
		// The retired `,` inside a group: text-detected in the legacy half,
		// refused by the parser (retired_comma_connective) in the v1 half.
		{dslgate.GateRetiredOperator, " && (true, true)"},
		// A bare row intrinsic. `id` is a payload read in a legacy filter and
		// an unbound name in a v1 one; the gate reports both.
		{dslgate.GateFilterRowIntrinsic, " && id == args.injectedId"},
	} {
		t.Run(string(tc.gate), func(t *testing.T) {
			bothCorpora(t, func(t *testing.T, c corpus) {
				inj, n := injectIntoFilters(c, tc.suffix)
				if n < 400 {
					t.Fatalf("injected into only %d filter clauses; the corpus walk has stopped finding them", n)
				}
				got := 0
				for _, v := range dslgate.ScanFiles(inj.sources(), opts) {
					if v.Gate == tc.gate {
						got++
					}
				}
				if got != n {
					t.Errorf("%s reported %d of the %d violations written into the %s corpus", tc.gate, got, n, c.name)
				}
				t.Logf("%s caught %d of %d injected violations", tc.gate, got, n)
			})
		})
	}
}

// TestAdminCompositionCatchesOnTheRealCorpus turns every filter's gate
// conjunct into a disjunct, in both editions, and requires the composition
// gate to refuse the same constructs in each. Not every injected clause is a
// violation -- `owner || isClusterOwner` is the composite tier's own floor --
// so the assertion is parity plus a floor rather than a count.
func TestAdminCompositionCatchesOnTheRealCorpus(t *testing.T) {
	opts := serverOnlyOptions(t)
	refused := func(c corpus) map[string]bool {
		inj, _ := injectIntoFilters(c, " || actor.isClusterOwner == true")
		out := map[string]bool{}
		for _, v := range dslgate.ScanFiles(inj.sources(), opts) {
			if v.Gate == dslgate.GateAdminGateComposition {
				out[v.File+" "+v.Construct] = true
			}
		}
		return out
	}
	legacy, v1 := refused(embeddedCorpus(t)), refused(migratedCorpus(t))
	for key := range legacy {
		if !v1[key] {
			t.Errorf("%s: an admin gate composed as a disjunct is refused in the embedded tree and passed once migrated", key)
		}
	}
	for key := range v1 {
		if !legacy[key] {
			t.Errorf("%s: an admin gate composed as a disjunct is refused only once migrated", key)
		}
	}
	// Measured when this landed: 146 constructs refused in each edition.
	if len(legacy) < 100 {
		t.Errorf("only %d constructs refused with their admin gate made a disjunct; the composition gate has stopped catching", len(legacy))
	}
	t.Logf("%d constructs refused in both editions", len(legacy))
}

func TestUserScopeGateCatchesOnTheRealCorpus(t *testing.T) {
	opts := serverOnlyOptions(t)
	legacy := classifyCorpus(injectCallerArg(embeddedCorpus(t)), opts)
	v1 := classifyCorpus(injectCallerArg(migratedCorpus(t)), opts)

	flagged := 0
	for key, want := range legacy {
		if got := v1[key]; got != want {
			t.Errorf("%s: with its caller check replaced by a caller-supplied value, classifies %s in the embedded tree and %s once migrated", key, want, got)
		}
		if want == dslgate.BucketFlagged {
			flagged++
		}
	}
	// Measured when this landed: 145 constructs flag in each edition. This is
	// the reachable positive for the gate's CATCH half on a corpus that is
	// otherwise clean.
	if flagged < 100 {
		t.Errorf("only %d constructs flag once every caller check is replaced by a caller-supplied value; the user-scope gate has stopped catching", flagged)
	}
	t.Logf("%d constructs flag in both editions once their caller check is removed", flagged)
}

// TestCrossNamespaceImportGateCatchesOnTheRealCorpus strips every file-top
// `use` line from both corpora: every cross-namespace reference is then
// unimported, and the gate must report the same ones in both editions -- a v1
// trait use is a call, `isX(row)`, and a v1 spec or trait is declared with no
// brace, so a gate that read either the old way would lose them.
func TestCrossNamespaceImportGateCatchesOnTheRealCorpus(t *testing.T) {
	useLine := regexp.MustCompile(`(?m)^[ \t]*use[ \t]+[^\n]*$`)
	opts := serverOnlyOptions(t)
	reported := func(c corpus) map[string]bool {
		files := make(map[string]string, len(c.files))
		for p, s := range c.files {
			files[p] = useLine.ReplaceAllString(s, "")
		}
		out := map[string]bool{}
		for _, v := range dslgate.ScanFiles(newCorpus(c.name+"+unimported", files).sources(), opts) {
			if v.Gate == dslgate.GateCrossNamespaceImport {
				out[v.File+" "+v.Detail] = true
			}
		}
		return out
	}
	legacy, v1 := reported(embeddedCorpus(t)), reported(migratedCorpus(t))
	for key := range legacy {
		if !v1[key] {
			t.Errorf("reported in the embedded tree, not once migrated: %s", key)
		}
	}
	for key := range v1 {
		if !legacy[key] {
			t.Errorf("reported only once migrated: %s", key)
		}
	}
	// Measured when the floor was set: 129 in each edition.
	if len(legacy) < 90 {
		t.Errorf("only %d unimported references reported -- the gate has stopped reading references or declarations", len(legacy))
	}
	t.Logf("%d unimported cross-namespace references reported in both editions", len(legacy))
}
