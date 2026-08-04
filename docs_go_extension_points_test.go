package main

import (
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestDocsDoNotTeachDeletedGoExtensionPoints is the third gate in the
// docs-drift family, alongside TestDocsDoNotReferencePrefixedConstructNames
// (#2914 / #2917 / #2979) and TestDocsDoNotTeachRetiredDeclarationKeywords
// (#2974). Those two police the DSL surface; this one polices the Go surface
// a doc presents as a plug-in extension point (memql#2967).
//
// # The class
//
// A doc names a Go symbol as registerable, the symbol is deleted, and the doc
// keeps teaching it. Nothing notices.
//
// The worked example is `node.RegisterConceptOwnership`, deleted with
// component/node/query_proxy.go in ac3a751e on 2026-05-16. Three docs
// presented it as registerable for 74 days, next to `node.RegisterRoutingRule`,
// which is real -- the dead half read as authoritative because its neighbour
// was.
//
// It was not inert. PR #2919 argued against deleting the query-forwarding
// machinery *because* of it ("a documented plug-in extension point (CLAUDE.md),
// so the alternative -- deleting it -- would remove published architecture
// rather than fix it"), an argument resting entirely on a doc describing
// something that had not existed for two months.
//
// # Why the allowlist is the whole design
//
// memql#2966 declined to build this gate, arguing that "docs must not name a
// `node.Register*` symbol absent from Go" cannot tell teaching a dead primitive
// from correctly recording that it is gone -- the corrected prose necessarily
// still contains the identifier. That is true of the naive gate, and a
// heuristic guessing which mentions are negative would be worse than nothing.
//
// The gate plus an explicit RETIRED allowlist is exact rather than heuristic.
// #2966's own corrected prose passes (RegisterConceptOwnership is in RETIRED,
// citing the commit that removed it) and a future invented `node.RegisterFoo`
// fails -- which is precisely the split the naive gate could not make.
//
// The allowlist is also what would have prevented the SECOND failure here.
// docs/internal/design/extension-points.md caught the drift and wrote "the
// CLAUDE.md line is stale and should be corrected separately (tracked as a doc
// follow-up)". The follow-up was never filed. Adding a RETIRED entry forces you
// to name the removing commit, which makes "tracked as a doc follow-up"
// concrete instead of aspirational -- and retiredGoSymbolsAreActuallyGone below
// forces the entry back out again once it stops being true.
//
// # Resolution is by NAME, not by qualifier
//
// A doc writes `engine.RegisterIntegration` and `dsl.RegisterTree`; neither
// qualifier is a package path. `engine` is a RECEIVER VARIABLE (the method is
// component/memql/engine_ai.go:38, called at app/plugins.go:53) and `dsl` /
// `memqldsl` are IMPORT ALIASES of the same dsl/embed.go:86 function. A gate
// matching only `func <Name>` in a package named by the qualifier flags all
// three as missing, which is how a gate gets deleted for crying wolf.
//
// So the resolver asks only "does any Go file declare this name, as a function
// or as a method". STATED BOUNDARY: that cannot catch a doc naming a live
// symbol under the WRONG package. That is a different and much narrower defect
// than the one here -- a wrong qualifier still points at something that exists,
// while this class points at nothing at all -- and closing it needs real import
// resolution rather than a scan. Not attempted; recorded so the next reader
// does not assume it is covered.
//
// # Qualified mentions only
//
// The pattern requires the `<qualifier>.` prefix, and that is load-bearing for
// the same reason the allowlist is. A bare `RegisterConceptOwnership` in a
// sentence is almost always prose ABOUT the symbol; the qualified form reads as
// a call you can make. Two of the five surviving markdown mentions are
// qualified and three are bare, and the split matches that reading exactly.
//
// # Scope: Register* only, measured
//
// memql#2967's last checklist item asks whether to widen past `Register*` to
// any exported symbol a doc presents as callable. Measured on this tree rather
// than guessed: the qualified-identifier shape `<qualifier>.<Exported>` matches
// far beyond Go (`args.Payload`, `Bundle.Nodes`, proto and TypeScript members,
// prose like `Timescale.Cloud`), so the same rule widened produces a wall of
// findings that are not Go symbols at all. Separating them needs a language
// model of the surrounding fence, not a wider regex.
//
// `Register*` is the tractable subset because it names a REGISTRATION verb --
// a doc writing it is making a claim about an extension point, which is the
// class #2919 was misled by. Widening belongs with a fence-aware scanner, and
// shipping it half-done would mean either a noisy gate or a long exemption
// list, both of which end with the gate being ignored. Recorded here rather
// than filed as a follow-up nobody picks up, which is the failure mode this
// gate exists to punish.
//
// # PR-time coverage: the same hole both siblings have
//
// .github/workflows/ci.yml routes lanes by changed-path bucket for
// `pull_request`, and no bucket matches `**/*.md`, so a docs-only PR skips
// go-checks and never runs this gate. Drift still cannot LAND undetected --
// `merge_group` runs every lane unconditionally -- but the feedback arrives at
// the merge queue rather than on the PR. Identical to the two sibling gates and
// already tracked as memql#3054; noted rather than re-filed.
func TestDocsDoNotTeachDeletedGoExtensionPoints(t *testing.T) {
	declared := declaredGoFuncNames(t)

	// The resolver is the half that can fail SILENTLY: if the Go scan comes
	// back empty or narrowed, every mention is unresolvable and the gate goes
	// loud, which is safe. The dangerous direction is the opposite -- a
	// resolver that over-matches would report everything live and never fire.
	// Assert a known method and a known plain function by name, since the
	// method arm is the one a "simplify the regex" edit removes first.
	for _, sentinel := range []string{
		"RegisterRoutingRule", // plain func, component/node/routing.go
		"RegisterIntegration", // METHOD, component/memql/engine_ai.go
		"RegisterTree",        // reached through import aliases dsl / memqldsl
	} {
		if !declared[sentinel] {
			t.Fatalf("the Go resolver did not find %q, which this repo does declare -- the scan "+
				"has broken and this gate would now fail on every live symbol", sentinel)
		}
	}

	out, err := exec.Command("git", "ls-files", "-z", "*.md").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Split(string(out), "\x00")

	var scanned, mentions int
	seen := map[string]bool{}
	resolved := map[string]bool{}

	for _, rel := range files {
		if rel == "" {
			continue
		}
		data, readErr := os.ReadFile(rel)
		if readErr != nil {
			// A tracked-but-absent path is somebody else's problem; skipping it
			// silently is what would be wrong, so say so.
			t.Logf("skipping unreadable tracked file %s: %v", rel, readErr)
			continue
		}
		scanned++
		seen[rel] = true

		for i, line := range strings.Split(string(data), "\n") {
			for _, m := range qualifiedRegisterRE.FindAllStringSubmatch(line, -1) {
				qualifier, symbol := m[1], m[2]
				mentions++
				if declared[symbol] {
					resolved[symbol] = true
					continue
				}
				if _, ok := retiredGoSymbols[symbol]; ok {
					resolved[symbol] = true
					continue
				}
				t.Errorf("%s:%d presents `%s.%s` as a Go extension point, but nothing in this "+
					"repo declares `%s` -- not as a function and not as a method.\n"+
					"  %s\n"+
					"A doc naming a deleted registration hook reads as authoritative next to the "+
					"live ones around it, and memql#2919 argued a design decision from exactly "+
					"that. Either the symbol is misspelled, or it was removed and this doc needs "+
					"rewriting. If the doc is CORRECTLY recording that the symbol is gone, add it "+
					"to retiredGoSymbols with the commit that removed it -- naming the commit is "+
					"the point, and retiredGoSymbolsAreActuallyGone forces the entry out again if "+
					"the symbol ever comes back.",
					rel, i+1, qualifier, symbol, symbol, strings.TrimRight(line, "\r"))
			}
		}
	}

	// A sweep that stops resolving files reports clean forever. A count alone
	// is not enough -- the sibling gate measured that narrowing to `docs/*.md`
	// still leaves 123 files while dropping CLAUDE.md, which is where this
	// issue's worked example lived -- so assert the specific files and the
	// specific symbols this gate exists for.
	if scanned < 50 {
		t.Fatalf("only %d markdown files scanned -- the sweep has stopped resolving them and this "+
			"gate would now pass vacuously", scanned)
	}
	if mentions < 5 {
		t.Fatalf("only %d qualified Register* mentions found across %d markdown files -- the "+
			"pattern has stopped matching and this gate would now pass vacuously",
			mentions, scanned)
	}
	for _, sentinel := range []string{
		"CLAUDE.md",
		"integrations/arch.md",
		"docs/internal/design/extension-points.md",
	} {
		if !seen[sentinel] {
			t.Fatalf("%s was not scanned (%d files were) -- the sweep has narrowed away from the "+
				"docs that carried memql#2967's worked example. A file count cannot catch that, "+
				"which is why these sentinels are asserted by name.", sentinel, scanned)
		}
	}
	for _, sentinel := range []string{
		"RegisterRoutingRule",      // the live neighbour that lent the dead one its authority
		"RegisterConceptOwnership", // the retired one
	} {
		if !resolved[sentinel] {
			t.Fatalf("`%s` was not matched in any markdown file -- either the docs stopped "+
				"mentioning it or the pattern stopped matching it. The second is a silent "+
				"disable, so this asserts the gate is still looking at the pair memql#2967 was "+
				"filed about.", sentinel)
		}
	}
}

// qualifiedRegisterRE matches `<qualifier>.Register<Name>` -- the shape that
// reads as "a registration you can call".
//
// The qualifier arm allows an initial capital on purpose: `Service.RegisterRoutes`
// in docs/ names a TYPE, not a package, and it is a real method. Requiring
// lowercase would silently skip it, and a skipped mention is indistinguishable
// from a passing one.
//
// ONE definition, shared by the sweep and the fixture test below. The sibling
// gate learned this the hard way (memql#3044 review): a pin test carrying its
// own copy of the pattern pins a string that exists only inside itself, so
// sabotaging the sweep's copy left both the fixture and the suite green.
var qualifiedRegisterRE = regexp.MustCompile(
	`\b([A-Za-z_][A-Za-z0-9_]*)\.(Register[A-Z][A-Za-z0-9_]*)\b`)

// goFuncDeclRE matches a top-level Go function OR method declaration and
// captures the name.
//
// The optional receiver group is not cosmetic: `RegisterIntegration` and
// `RegisterRoutes` are methods, and a pattern without it reports both as
// deleted. That is the crying-wolf failure memql#2967 called out by name.
// shaShapedRE is the always-available half of the removedIn check: a commit
// id is hex and at least 7 characters, which rejects "TODO" / "see #2966" /
// "unknown" even on a shallow clone where history cannot be consulted.
var shaShapedRE = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

var goFuncDeclRE = regexp.MustCompile(
	`^func\s+(?:\([^)]*\)\s*)?([A-Z][A-Za-z0-9_]*)\s*(?:\[[^\]]*\]\s*)?\(`)

// declaredGoFuncNames is every exported function/method name declared anywhere
// in the git-tracked Go of this repo.
//
// By NAME, deliberately -- see the doc comment on the test above for why the
// qualifier cannot be trusted and what that boundary does not cover.
func declaredGoFuncNames(t *testing.T) map[string]bool {
	t.Helper()

	out, err := exec.Command("git", "ls-files", "-z", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files *.go: %v", err)
	}

	names := map[string]bool{}
	var scanned int
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" {
			continue
		}
		// A symbol declared ONLY in a _test.go is not an extension point, and a
		// doc presenting one as registerable is exactly the "reads as
		// authoritative because its neighbour is" failure this gate exists to
		// catch -- resolving against test files would green-light it.
		//
		// This also closes the resolver's one over-matching seam. goFuncDeclRE
		// is line-based, so a `func Foo()` written inside a raw-string fixture
		// reads as a declaration; every such phantom in this tree lives in a
		// _test.go, so the two arms close together.
		if strings.HasSuffix(rel, "_test.go") || strings.HasPrefix(rel, "vendor/") {
			continue
		}
		data, readErr := os.ReadFile(rel)
		if readErr != nil {
			t.Logf("skipping unreadable tracked file %s: %v", rel, readErr)
			continue
		}
		scanned++
		for _, line := range strings.Split(string(data), "\n") {
			if m := goFuncDeclRE.FindStringSubmatch(line); m != nil {
				names[m[1]] = true
			}
		}
	}
	if scanned < 100 {
		t.Fatalf("only %d Go files scanned -- the resolver has stopped resolving them, and every "+
			"documented symbol would now report as deleted", scanned)
	}
	return names
}

// retiredGoSymbols is the allowlist: symbols a doc may name because the doc is
// recording that they are GONE, not teaching them.
//
// Every entry names the commit that removed the symbol. That is the whole
// mechanism -- an entry you cannot justify with a commit is an entry you should
// not be adding, and writing the commit down is what turns "tracked as a doc
// follow-up" into something concrete. memql#2967 exists because that follow-up
// was written as prose and never filed.
//
// The list is meant to stay short. It is not an exemption list for docs you do
// not want to fix: a doc TEACHING a dead symbol should be rewritten, and only a
// doc explaining its absence belongs here.
var retiredGoSymbols = map[string]struct{ removedIn, note string }{
	"RegisterConceptOwnership": {
		removedIn: "ac3a751e",
		note: "deleted with component/node/query_proxy.go on 2026-05-16 " +
			"(\"simplify: drop per-node @visibility filtering + concept-ownership routing\"). " +
			"Three docs kept teaching it for 74 days and memql#2919 argued a design decision " +
			"from one of them; the surviving mentions record its absence (memql#2966).",
	},
}

// A RETIRED entry that stops being true is worse than no entry: it silently
// excuses a live symbol from the gate, and nothing else would notice.
//
// This is the direct analogue of the stale-exemption check on memql#2982's
// owner gate, and it is what makes the allowlist self-cleaning rather than a
// growing pile of grandfathering.
func TestRetiredGoSymbolsAreActuallyGone(t *testing.T) {
	declared := declaredGoFuncNames(t)

	// Non-empty is not the same as real. Three places in this file say naming
	// the commit is "the whole mechanism"; checking only for the empty string
	// lets `removedIn: "TODO"` or `"see #2966"` ship, and the mechanism is then
	// decorative. Two checks, because they hold in different places:
	//
	//   shape   -- always. A SHA is hex and at least 7 characters, so the
	//              prose spellings are rejected everywhere, including CI.
	//   history -- only where history exists. CI checks out with actions/
	//              checkout's default fetch-depth of 1, so a SHALLOW clone has
	//              exactly one commit and `git cat-file -e` fails for every
	//              real SHA. Verifying unconditionally does not tighten the
	//              gate there, it just makes it fail on a correct entry -- so
	//              the existence check is skipped when the clone is shallow,
	//              loudly, rather than turning a true record into a red build.
	shallow := false
	if out, err := exec.Command("git", "rev-parse", "--is-shallow-repository").Output(); err == nil {
		shallow = strings.TrimSpace(string(out)) == "true"
	}
	if shallow {
		t.Logf("shallow clone: verifying removedIn SHAs by shape only, not against history")
	}

	var stale []string
	for symbol, entry := range retiredGoSymbols {
		switch {
		case entry.removedIn == "":
			t.Errorf("retiredGoSymbols[%q] names no removing commit. The commit IS the "+
				"mechanism -- without it the entry is an exemption rather than a record.", symbol)
		case !shaShapedRE.MatchString(entry.removedIn):
			t.Errorf("retiredGoSymbols[%q].removedIn is %q, which is not shaped like a commit "+
				"(hex, at least 7 characters). An entry that cannot be justified with a real "+
				"commit is an exemption wearing a record's clothes -- which is the failure "+
				"memql#2967 was filed about, one level up.", symbol, entry.removedIn)
		case !shallow:
			if err := exec.Command("git", "cat-file", "-e", entry.removedIn+"^{commit}").Run(); err != nil {
				t.Errorf("retiredGoSymbols[%q].removedIn is %q, which is hex-shaped but is not a "+
					"commit in this repository.", symbol, entry.removedIn)
			}
		}
		if declared[symbol] {
			stale = append(stale, symbol)
		}
	}
	sort.Strings(stale)
	for _, symbol := range stale {
		t.Errorf("retiredGoSymbols[%q] says the symbol is gone, but this repo declares it again. "+
			"Remove the entry: while it sits there the gate cannot tell a doc teaching %q from a "+
			"doc recording its absence, which is the exact distinction the allowlist exists to "+
			"make.", symbol, symbol)
	}
}

// The gate must actually fire, and must not fire on the live surface. Without
// this the pattern could be wrong in a way that matches nothing and the sweep
// would report clean forever -- the silent-disable shape the whole family of
// doc gates is written to avoid.
//
// It exercises qualifiedRegisterRE, the SAME variable the sweep uses, so this
// is a pin rather than a self-referential assertion.
func TestDeletedGoSymbolGateMatchesAQualifiedCallAndNotProse(t *testing.T) {
	for _, bad := range []string{
		"node.RegisterNonexistent",                       // memql#2967's required synthetic case
		"Call `node.RegisterRoutingRule` from `init()`.", // inside prose + backticks
		"  memql.RegisterPlugin(name, factory)",          // indented code
		"see planner.RegisterContainerExecutor for the",  // mid-sentence
		"Service.RegisterRoutes(mux)",                    // TYPE qualifier, not a package
	} {
		if !qualifiedRegisterRE.MatchString(bad) {
			t.Errorf("the gate does not match a qualified registration mention, so it would "+
				"report clean forever: %q", bad)
		}
	}

	for _, ok := range []string{
		"There is no concept-ownership registry.", // prose with no symbol
		"RegisterConceptOwnership existed once",   // BARE -- prose about the symbol
		"register the plugin from init()",         // lowercase verb
		"node.registerRoutingRule",                // unexported: not an extension point
		"the Registry holds every routing rule",   // `Register` is not a prefix here
		"node.Register",                           // no <Name> segment
		"config.RegistryURL",                      // Registry*, not Register<Name>
	} {
		if qualifiedRegisterRE.MatchString(ok) {
			t.Errorf("the gate fires on a legitimate line, which is how a gate gets suppressed "+
				"and then stops catching the real case: %q", ok)
		}
	}
}

// The resolver's method arm and its exported-only rule, pinned separately from
// the pattern. Both are the kind of thing a later simplification removes
// without any test going red -- the method arm because plain functions are the
// common case, the exported rule because it looks like an accident.
func TestGoFuncResolverSeesMethodsAndSkipsUnexported(t *testing.T) {
	for _, decl := range []struct{ line, want string }{
		{"func RegisterRoutingRule(rule RoutingRule) {", "RegisterRoutingRule"},
		{"func (e *MemQLEngine) RegisterIntegration(provider IntegrationProvider) error {", "RegisterIntegration"},
		{"func (s *Service) RegisterRoutes(mux *http.ServeMux) {", "RegisterRoutes"},
		{"func RegisterTree(domain string, tree fs.FS) {", "RegisterTree"},
		{"func Map[K comparable, V any](in map[K]V) []V {", "Map"}, // generic
	} {
		m := goFuncDeclRE.FindStringSubmatch(decl.line)
		if m == nil {
			t.Errorf("resolver missed a declaration it must see, so the symbol would report as "+
				"deleted: %q", decl.line)
			continue
		}
		if m[1] != decl.want {
			t.Errorf("resolver read %q as %q, want %q", decl.line, m[1], decl.want)
		}
	}

	for _, line := range []string{
		"func registerRoutingRule(rule RoutingRule) {", // unexported
		"\tfunc nested() {}",                           // not top level
		"// func RegisterGhost(x int) {",               // commented out
		"type RegisterFunc func(string)",               // a type, not a declaration
	} {
		if m := goFuncDeclRE.FindStringSubmatch(line); m != nil {
			t.Errorf("resolver treated %q as declaring %q -- an over-matching resolver reports "+
				"deleted symbols as live and the gate silently stops firing", line, m[1])
		}
	}
}
