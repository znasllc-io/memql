package memql

// memql#3082 -- memql#3026 DoD item 5, second clause:
//
//	the scanner skips comments, AND dsl/deployment carries a real canonicalId
//	call so the path is live-tested.
//
// The scanner half shipped in memql#3080. This is the second clause, and the
// decision it asked for is OPTION 3: accept the fixture + composition-test
// coverage rather than pay for a live call.
//
// Options 1 and 2 both buy a live regression guard at real cost. Option 1
// (re-key): createDeploymentNodeSpec / updateDeploymentNodeSpec derive the row
// id as hash(concat(shortId(args.deploymentId), ":", args.nodeType)); shortId is
// bare-out (memql#1859) while canonicalId produces the prefixed form, so
// substituting it changes the string fed to hash() and EVERY derived id changes
// -- re-keying live rows against a design that says "No id changes". Option 2 is
// a schema change to a shipped concept. Option 3 produces no diff at all.
//
// But option 3 alone leaves the issue's closing sentence true -- "dsl/deployment
// should stop being the only place where a pinned domain's ambient path is
// untested by anything the loader actually walks." So this gate is the
// compensating control: it does not create coverage, it makes the coverage
// RELATIONSHIP explicit and enforced. Today dsl/deployment satisfies the
// composition-test arm. The gate fires the moment a SECOND pin appears with
// neither arm, which is the actual regression risk and the one nobody would
// otherwise notice.

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

// pinnedDomainCompositionCoverage is arm B: the pin -> the composition test that
// exercises it through the PRODUCTION entry point.
//
// Keyed by PIN rather than by domain, because the pin is what the remapped
// ambient path is about -- a domain whose directory differs from the namespace
// its concepts assemble under. The value is a test NAME and it is verified to
// exist, so deleting the test breaks this gate instead of silently removing the
// coverage it stands for. That is the requirement that makes naming it worth
// anything.
var pinnedDomainCompositionCoverage = map[string]string{
	// dsl/deployment pins "cluster". TestCanonicalId_InDomainComposesTheRealPin
	// resolves `canonicalId(args.x, deployment)` through the two-argument
	// ResolveCanonicalIdConceptRefsInDomain -- the only form production calls --
	// against the REAL pin, with an ambiguous registry so a unique-name
	// resolution cannot pass for the right reason by accident.
	"cluster": "TestCanonicalId_InDomainComposesTheRealPin",
}

// TestEveryPinnedDomainHasAmbientPathCoverage is the gate.
//
// It lives beside the other canonicalId corpus assertions rather than in the
// loader, because it is a statement about the TREE, not a load-time rule.
func TestEveryPinnedDomainHasAmbientPathCoverage(t *testing.T) {
	pins := pinnedDomainsInTree(t)
	if len(pins) == 0 {
		t.Fatal("no namespace.pin found anywhere in the tree -- either the pin mechanism was removed (in which case this gate and the ambient declared-namespace rule should go with it) or the enumeration is broken")
	}
	// Enumeration pinned against the known tree, so a gate that finds nothing
	// cannot pass by vacuously iterating an empty set. dsl/deployment is the
	// tree's only pin at land time (memql#3082); a NEW pin appearing here is
	// exactly the event this gate exists to notice, and it will be reported
	// below by name rather than by this line.
	if got := pins["deployment"]; got != "cluster" {
		t.Errorf("enumeration must read dsl/deployment/namespace.pin through the loader's own reader: got %q, want \"cluster\"", got)
	}

	live := liveCanonicalIdCallsByDomain(t)
	tests := testFunctionsInPackage(t)

	domains := make([]string, 0, len(pins))
	for d := range pins {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	for _, domain := range domains {
		pin := pins[domain]
		arm, detail := coveringArm(domain, pin, live, pinnedDomainCompositionCoverage)
		if arm == "" {
			t.Errorf("pinned domain %q (namespace.pin -> %q) has NO ambient-path coverage.\n"+
				"  A pinned domain is one whose directory differs from the namespace its concepts\n"+
				"  assemble under, so it is the ONLY shape that exercises the remapped ambient path\n"+
				"  (memql#2976 / #3017 / #3026). Give it one of:\n"+
				"    arm A -- a live canonicalId call in dsl/%s/, so the corpus itself covers it; or\n"+
				"    arm B -- an entry in pinnedDomainCompositionCoverage naming a composition test\n"+
				"             that resolves through the production entry point against this pin.\n"+
				"  %s", domain, pin, domain, detail)
			continue
		}
		// Naming the arm is an acceptance criterion, not decoration: a reader
		// has to be able to see that dsl/deployment is covered by a test rather
		// than by the corpus, without going and checking.
		t.Logf("pinned domain %-12q pin=%-10q covered by %s (%s)", domain, pin, arm, detail)
	}

	// Verify arm B's named tests actually exist. A dangling name would let this
	// gate report coverage that has been deleted -- the exact failure mode the
	// "reference it by name" requirement exists to prevent.
	for pin, name := range pinnedDomainCompositionCoverage {
		body, ok := tests[name]
		if !ok {
			t.Errorf("pinnedDomainCompositionCoverage names %q as the composition-test coverage for pin %q, but no such test exists in this package.\n"+
				"  Either the test was deleted (restore it, or move that pin to arm A) or it was renamed (update the map).", name, pin)
			continue
		}
		// And that it still concerns THIS pin. A test that stops mentioning the
		// pin has stopped covering it, however healthy its name looks.
		if !strings.Contains(body, pin) {
			t.Errorf("composition test %q no longer mentions the pin %q it is registered as covering -- it may have been rewritten to test something else", name, pin)
		}
	}

	// The measurement, re-derived at land time and recorded (acceptance
	// criterion). The issue said 19 live calls across `cognition` and `library`;
	// re-derived here with comments and string literals stripped, it is ELEVEN,
	// all of them in dsl/cognition/mutations.memql -- one domain, one file. The
	// higher figures counted comment occurrences: the token appears 20 times
	// across four files, and 9 of those are prose (4 in cognition/mutations, 2
	// in cognition/queries, 1 in deployment/mutations, 2 in library/automations).
	//
	// The conclusion holds and is stronger than filed: dsl/deployment is the
	// tree's only pin and carries zero live calls, `library` carries zero too,
	// so ZERO of the 11 live calls exercise the remapped-ambient path.
	total := 0
	summary := make([]string, 0, len(live))
	for domain, n := range live {
		total += n
		summary = append(summary, domain+"="+strconv.Itoa(n))
	}
	sort.Strings(summary)
	t.Logf("live canonicalId calls: %d total, by domain: %v", total, summary)
	if total == 0 {
		t.Error("no live canonicalId call anywhere in the tree -- arm A is unreachable for every domain, which makes this gate weaker than it reads; check that liveCanonicalIdCallsByDomain still recognises the call form")
	}
	for domain := range pins {
		if live[domain] > 0 {
			t.Logf("NOTE: pinned domain %q now carries %d live canonicalId call(s) -- the corpus covers the remapped ambient path directly, and memql#3082's option-3 rationale can be revisited", domain, live[domain])
		}
	}
}

// coveringArm decides which arm covers one pinned domain, and is a PURE
// function of its inputs so the gate's own firing can be tested without
// planting a fixture pin in the real tree (acceptance criterion: "a test proves
// the gate fires").
func coveringArm(domain, pin string, live map[string]int, composition map[string]string) (arm, detail string) {
	if n := live[domain]; n > 0 {
		return "arm A (live corpus call)", strconv.Itoa(n) + " live canonicalId call(s) in dsl/" + domain + "/"
	}
	if name, ok := composition[pin]; ok {
		return "arm B (composition test)", name
	}
	return "", "dsl/" + domain + "/ carries no live canonicalId call, and no composition test is registered for pin " + pin
}

// TestPinnedDomainCoverageGateFires proves the gate is capable of failing.
//
// A gate nobody has watched go red is a gate nobody knows works -- and in this
// area that is not a general principle but the specific way memql#3043 and
// memql#3026 both survived: the assertion existed and could not fire.
func TestPinnedDomainCoverageGateFires(t *testing.T) {
	live := map[string]int{"cognition": 11}
	composition := map[string]string{"cluster": "TestCanonicalId_InDomainComposesTheRealPin"}

	// The tree as it stands: one pin, covered by arm B.
	if arm, _ := coveringArm("deployment", "cluster", live, composition); arm == "" {
		t.Error("dsl/deployment must be covered by arm B today")
	}

	// A SECOND pinned domain with neither arm -- no live call in its directory,
	// no composition-test entry for its pin. This is the case the gate exists
	// for, and it must be refused.
	if arm, detail := coveringArm("beta", "gamma", live, composition); arm != "" {
		t.Errorf("a second pinned domain with neither arm must be uncovered, got arm %q (%s)", arm, detail)
	}

	// Each arm alone is sufficient.
	if arm, _ := coveringArm("beta", "gamma", map[string]int{"beta": 1}, composition); arm == "" {
		t.Error("arm A alone must cover a pin")
	}
	if arm, _ := coveringArm("beta", "gamma", live, map[string]string{"gamma": "TestSomething"}); arm == "" {
		t.Error("arm B alone must cover a pin")
	}
}

// pinnedDomainsInTree returns domain -> pin for every namespace.pin in the tree.
//
// It reads through memqldsl.Tree() and namespacePin -- the same FS and the same
// pin reader the loader uses -- so a runtime-mounted MEMQL_DSL_PATH domain's pin
// is enumerated exactly like a core domain's. A second source of truth for "what
// the pins are" is the thing this gate is least able to afford.
func pinnedDomainsInTree(t *testing.T) map[string]string {
	t.Helper()
	tree := memqldsl.Tree()
	entries, err := fs.ReadDir(tree, ".")
	if err != nil {
		t.Fatalf("read DSL tree root: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if pin := namespacePin(tree, e.Name()); pin != "" {
			out[e.Name()] = pin
		}
	}
	return out
}

// liveCanonicalIdCallsByDomain counts canonicalId CALLS -- not token
// occurrences -- per domain, with comments and string literals stripped.
//
// The stripping is restated here rather than shared with the resolver's scanner,
// for the reason test/dslconformance/callgraph_contract_test.go restates kindKeywords: a count
// derived from the implementation it is checking moves with that implementation
// and agrees with it while both are wrong. The distinction is not academic --
// the figure this replaces (19 calls across two domains) was produced by
// counting the token, and 9 of the tree's 20 occurrences are prose in comments.
func liveCanonicalIdCallsByDomain(t *testing.T) map[string]int {
	t.Helper()
	callRE := regexp.MustCompile(`\bcanonicalId\s*\(`)
	out := map[string]int{}
	tree := memqldsl.Tree()
	err := fs.WalkDir(tree, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != "." && (strings.HasPrefix(d.Name(), "_") || strings.HasPrefix(d.Name(), ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".memql") {
			return nil
		}
		raw, err := fs.ReadFile(tree, path)
		if err != nil {
			return err
		}
		n := len(callRE.FindAllString(stripMemqlCommentsAndStrings(string(raw)), -1))
		if n > 0 {
			out[firstSegment(path)] += n
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk DSL tree: %v", err)
	}
	return out
}

// stripMemqlCommentsAndStrings blanks every `//` comment, `/* */` comment and
// double-quoted string literal, preserving offsets so nothing else shifts.
// Both comment forms are handled because the lexer handles both -- covering only
// `//` is the defect memql#3080 fixed in the resolver's own scanner.
func stripMemqlCommentsAndStrings(s string) string {
	out := []byte(s)
	blank := func(from, to int) {
		for i := from; i < to && i < len(out); i++ {
			if out[i] != '\n' {
				out[i] = ' '
			}
		}
	}
	i, inStr := 0, false
	for i < len(s) {
		if inStr {
			// A backslash escape consumes the next byte whole, so an escaped
			// quote does not close the string.
			if s[i] == '\\' && i+1 < len(s) {
				blank(i, i+2)
				i += 2
				continue
			}
			if s[i] == '"' {
				inStr = false
				blank(i, i+1)
				i++
				continue
			}
			blank(i, i+1)
			i++
			continue
		}
		switch {
		case s[i] == '"':
			inStr = true
			blank(i, i+1)
			i++
		case strings.HasPrefix(s[i:], "//"):
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				blank(i, len(s))
				i = len(s)
				continue
			}
			blank(i, i+end)
			i += end
		case strings.HasPrefix(s[i:], "/*"):
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				blank(i, len(s)) // unterminated: runs to EOF, as skipBlockComment does
				i = len(s)
				continue
			}
			blank(i, i+2+end+2)
			i += 2 + end + 2
		default:
			i++
		}
	}
	return string(out)
}

// testFunctionsInPackage returns test name -> source text for every Test
// function declared in this package's directory, so arm B's named test can be
// proven to exist and to still concern its pin.
func testFunctionsInPackage(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("glob package test files: %v", err)
	}
	declRE := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]*)\(`)
	out := map[string]string{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(raw)
		locs := declRE.FindAllStringSubmatchIndex(src, -1)
		for i, loc := range locs {
			end := len(src)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			out[src[loc[2]:loc[3]]] = src[loc[0]:end]
		}
	}
	if len(out) == 0 {
		t.Fatal("found no Test functions in this package's sources -- the scan is broken, so arm B cannot be verified")
	}
	return out
}

// firstSegment returns a tree-relative path's leading directory, which is the
// domain a concept id is assembled from.
func firstSegment(p string) string {
	if i := strings.IndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return ""
}
