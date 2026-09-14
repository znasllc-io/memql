package dslconformance

// v1_contract_gates_test.go -- the load-time contract gates (dslgate) over the
// tree (epic memql#5363, task memql#5368).
//
// The engine refuses a boot on these gates, so what they say about the tree
// is what a node will say when it boots it. Two properties are asserted:
//
//  1. the tree is clean, and every row-accessing construct lands in a bucket,
//     with floors under the owned and admin buckets -- a classifier that
//     stopped reading filters would drop every construct into `other` and
//     report nothing;
//  2. each gate still CATCHES on the real tree: a violation is written into
//     every construct that could carry one, and each must be reported. The
//     tree is clean, so without this a blind gate would read as a gate with
//     nothing to report.

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

func TestContractGatesPassTheTree(t *testing.T) {
	opts := serverOnlyOptions(t)
	onTree(t, func(t *testing.T, c corpus) {
		for _, v := range dslgate.ScanFiles(c.sources(), opts) {
			t.Errorf("%s", v)
		}
	})
}

// TestAuthzClassificationReachesTheTree: the classifier reads every
// row-accessing construct's filter, and the owned and admin buckets hold what
// they held when the floors were set.
func TestAuthzClassificationReachesTheTree(t *testing.T) {
	classes := classifyCorpus(embeddedCorpus(t), serverOnlyOptions(t))
	counts := map[dslgate.Bucket]int{}
	for _, b := range classes {
		counts[b]++
	}
	// Floors: measured when these were set at 251 owned and 228 admin over
	// 1003 row-accessing constructs. A walk that stopped finding constructs,
	// or a classifier that stopped reading filters, drops through these.
	if counts[dslgate.BucketOwned] < 200 || counts[dslgate.BucketAdmin] < 150 || len(classes) < 800 {
		t.Errorf("classification floors: %d constructs, %d owned, %d admin -- the classifier is no longer reading what it used to", len(classes), counts[dslgate.BucketOwned], counts[dslgate.BucketAdmin])
	}
	t.Logf("%d constructs classified: %v", len(classes), counts)
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
// so the injected term composes with the whole clause: a long clause wraps
// across lines, and a term appended to its first line would bind to one
// conjunct rather than to the clause. It returns the new corpus and how many
// clauses were injected into.
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
// filter-reading contract gate, on the real tree: a violation is written into
// every filter clause the tree has, and each gate must report every one of
// them. The tree is clean, so without this a gate that went blind would read
// as a gate with nothing to report.
func TestContractGatesCatchOnTheRealCorpus(t *testing.T) {
	opts := serverOnlyOptions(t)
	for _, tc := range []struct {
		gate   dslgate.Gate
		suffix string
	}{
		// The retired `,` inside a group, refused by the parser
		// (retired_comma_connective).
		{dslgate.GateRetiredOperator, " && (true, true)"},
		// A bare row intrinsic: an unbound name in a lambda body.
		{dslgate.GateFilterRowIntrinsic, " && id == args.injectedId"},
	} {
		t.Run(string(tc.gate), func(t *testing.T) {
			onTree(t, func(t *testing.T, c corpus) {
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
// conjunct into a disjunct and requires the composition gate to refuse the
// constructs whose rows the disjunct opens. Not every injected clause is a
// violation -- `owner || isClusterOwner` is the composite tier's own floor --
// so the assertion is a floor rather than a count.
func TestAdminCompositionCatchesOnTheRealCorpus(t *testing.T) {
	inj, _ := injectIntoFilters(embeddedCorpus(t), " || actor.isClusterOwner == true")
	refused := map[string]bool{}
	for _, v := range dslgate.ScanFiles(inj.sources(), serverOnlyOptions(t)) {
		if v.Gate == dslgate.GateAdminGateComposition {
			refused[v.File+" "+v.Construct] = true
		}
	}
	// Measured when this landed: 146 constructs refused.
	if len(refused) < 100 {
		t.Errorf("only %d constructs refused with their admin gate made a disjunct; the composition gate has stopped catching", len(refused))
	}
	t.Logf("%d constructs refused", len(refused))
}

func TestUserScopeGateCatchesOnTheRealCorpus(t *testing.T) {
	flagged := 0
	for _, b := range classifyCorpus(injectCallerArg(embeddedCorpus(t)), serverOnlyOptions(t)) {
		if b == dslgate.BucketFlagged {
			flagged++
		}
	}
	// Measured when this landed: 145 constructs flag. This is the reachable
	// positive for the gate's CATCH half on a tree that is otherwise clean.
	if flagged < 100 {
		t.Errorf("only %d constructs flag once every caller check is replaced by a caller-supplied value; the user-scope gate has stopped catching", flagged)
	}
	t.Logf("%d constructs flag once their caller check is removed", flagged)
}

// TestCrossNamespaceImportGateCatchesOnTheRealCorpus strips every file-top
// `use` line from the tree: every cross-namespace reference is then
// unimported, and the gate must report them -- a trait use is a call,
// `isX(row)`, and a spec or trait is declared with no brace, so a gate that
// read either another way would lose them.
func TestCrossNamespaceImportGateCatchesOnTheRealCorpus(t *testing.T) {
	useLine := regexp.MustCompile(`(?m)^[ \t]*use[ \t]+[^\n]*$`)
	c := embeddedCorpus(t)
	files := make(map[string]string, len(c.files))
	for p, s := range c.files {
		files[p] = useLine.ReplaceAllString(s, "")
	}
	reported := map[string]bool{}
	for _, v := range dslgate.ScanFiles(newCorpus(c.name+"+unimported", files).sources(), serverOnlyOptions(t)) {
		if v.Gate == dslgate.GateCrossNamespaceImport {
			reported[v.File+" "+v.Detail] = true
		}
	}
	// Measured when the floor was set: 129.
	if len(reported) < 90 {
		t.Errorf("only %d unimported references reported -- the gate has stopped reading references or declarations", len(reported))
	}
	t.Logf("%d unimported cross-namespace references reported", len(reported))
}
