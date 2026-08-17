package scan

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The gate reported `197 reads ... no drift` while 62 variables were read
// through named constants and therefore invisible to it (memql#3818). These
// tests pin the two halves of the fix: constants FOLD, and what cannot fold is
// REPORTED.
//
// The second half is the one worth defending in a test. A scanner that folds
// constants and silently omits the residue produces a cleaner-looking number
// that is still clean only about what its mechanism happens to see -- the same
// defect one level up. So "unresolvable sites are populated, with the right
// file:line" is an assertion here, not a nice-to-have.

// writeGoFixture builds a throwaway root from a map of repo-relative path ->
// source and returns the root.
func writeGoFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, src := range files {
		writeFixtureFile(t, root, rel, src)
	}
	return root
}

// keysOf returns the sorted unique keys of an outcome's reads.
func keysOf(out Outcome) []string { return UniqueKeys(out.Reads) }

// TestConstantResolutionAcrossReadForms covers every shape the repo actually
// uses to name an env key, for all three read forms.
//
// Each case FAILS against the retired literal-only regexes: the argument is
// never a quoted string at the call site.
func TestConstantResolutionAcrossReadForms(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			// The shape from component/identity/http/badge_grant.go, which is
			// what made memql#3818 concrete: a single file-local const, read
			// through its name.
			name: "package-level const via os.Getenv",
			files: map[string]string{"component/x/a.go": `package x

import "os"

const envBadgeGrantPerHour = "MEMQL_IDENTITY_BADGE_GRANT_PER_HOUR"

func read() string { return os.Getenv(envBadgeGrantPerHour) }
`},
			want: []string{"MEMQL_IDENTITY_BADGE_GRANT_PER_HOUR"},
		},
		{
			name: "grouped const block via os.LookupEnv",
			files: map[string]string{"component/x/a.go": `package x

import "os"

const (
	envA = "MEMQL_FIXTURE_A"
	envB = "MEMQL_FIXTURE_B"
)

func read() (string, bool) {
	if v, ok := os.LookupEnv(envA); ok {
		return v, ok
	}
	return os.LookupEnv(envB)
}
`},
			want: []string{"MEMQL_FIXTURE_A", "MEMQL_FIXTURE_B"},
		},
		{
			// The component/config env* helper family -- 37 call sites in the
			// real tree, and the form the earlier 43-var measurement missed
			// entirely because it grepped os.Getenv only.
			name: "env* helper with a const first arg",
			files: map[string]string{"component/x/a.go": `package x

const EnvCacheTTLSeconds = "MEMQL_SAFETY_LLM_CACHE_TTL_SECONDS"

func envIntDefault(key string, def int) int { return def }

func read() int { return envIntDefault(EnvCacheTTLSeconds, 3600) }
`},
			want: []string{"MEMQL_SAFETY_LLM_CACHE_TTL_SECONDS"},
		},
		{
			// secret.EnvMasterKey (8 sites) / auth.EnvOperatorKey (4 sites):
			// the constant lives in another package and is reached through a
			// selector. Resolution goes via the file's import specs, so the
			// answer cannot come from a same-named constant in some unrelated
			// package.
			name: "cross-package qualified const",
			files: map[string]string{
				"component/auth/k.go": `package auth

const EnvOperatorKey = "MEMQL_OPERATOR_KEY"
`,
				"component/grpc/i.go": `package grpc

import (
	"os"

	"github.com/znasllc-io/memql/component/auth"
)

func read() string { return os.Getenv(auth.EnvOperatorKey) }
`,
				"go.mod": "module github.com/znasllc-io/memql\n\ngo 1.26.1\n",
			},
			want: []string{"MEMQL_OPERATOR_KEY"},
		},
		{
			name: "cross-package const through an import alias",
			files: map[string]string{
				"component/secret/k.go": `package secret

const EnvMasterKey = "MEMQL_MASTER_KEY"
`,
				"app/boot.go": `package app

import (
	"os"

	sec "github.com/znasllc-io/memql/component/secret"
)

func read() string { return os.Getenv(sec.EnvMasterKey) }
`,
				"go.mod": "module github.com/znasllc-io/memql\n\ngo 1.26.1\n",
			},
			want: []string{"MEMQL_MASTER_KEY"},
		},
		{
			name: "sibling file in the same package declares the const",
			files: map[string]string{
				"component/x/names.go": `package x

const envObserveLevel = "MEMQL_OBSERVE_LEVEL"
`,
				"component/x/read.go": `package x

import "os"

func read() string { return os.Getenv(envObserveLevel) }
`,
			},
			want: []string{"MEMQL_OBSERVE_LEVEL"},
		},
		{
			name: "function-local const and short declaration",
			files: map[string]string{"component/x/a.go": `package x

import "os"

func read() (string, string) {
	const local = "MEMQL_FIXTURE_LOCAL"
	short := "MEMQL_FIXTURE_SHORT"
	return os.Getenv(local), os.Getenv(short)
}
`},
			want: []string{"MEMQL_FIXTURE_LOCAL", "MEMQL_FIXTURE_SHORT"},
		},
		{
			// A folded concatenation of two constants is still a known key.
			// (A concatenation with a PARAMETER is not, and lands in the
			// residual instead -- see TestUnresolvableSitesAreReported.)
			name: "constant concatenation folds",
			files: map[string]string{"component/x/a.go": `package x

import "os"

const (
	prefix = "MEMQL_FIXTURE_"
	suffix = "SUFFIXED"
	full   = prefix + suffix
)

func read() string { return os.Getenv(full) }
`},
			want: []string{"MEMQL_FIXTURE_SUFFIXED"},
		},
		{
			name: "literal argument still resolves",
			files: map[string]string{"component/x/a.go": `package x

import "os"

func read() string { return os.Getenv("MEMQL_FIXTURE_LITERAL") }
`},
			want: []string{"MEMQL_FIXTURE_LITERAL"},
		},
		{
			// A constant that resolves to something which is not an env key is
			// not a read -- and is not a residual either, because the key IS
			// known. Same treatment the literal patterns gave it.
			name: "const folding to a non-env-shaped value is not a read",
			files: map[string]string{"component/x/a.go": `package x

func envSuffix(name string) string { return name }

const sep = "-"

func read() string { return envSuffix(sep) }
`},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ScanReads(writeGoFixture(t, tc.files))
			if err != nil {
				t.Fatalf("ScanReads: %v", err)
			}
			got := keysOf(out)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("reads = %v, want %v.\nAn empty result is the memql#3818 defect: the key is "+
					"named by a constant rather than a literal, which is statically resolvable and "+
					"was simply not resolvable BY A REGEX.", got, tc.want)
			}
			for _, u := range out.Unresolvable {
				t.Errorf("unexpected residual %s: every site in this fixture is resolvable, so a "+
					"residual here means the resolver gave up where it should have folded", u)
			}
		})
	}
}

// TestUnresolvableSitesAreReported is the assertion the issue turns on: a site
// whose key is a parameter yields a RESIDUAL with its location, and no read.
//
// The failure mode it guards is not a crash. It is a scanner that quietly
// returns nothing for these sites and prints a total that reads as coverage --
// `241 reads, no drift` -- which is the original defect wearing a bigger
// number.
func TestUnresolvableSitesAreReported(t *testing.T) {
	const src = `package x

import "os"

const envKnown = "MEMQL_FIXTURE_KNOWN"

func resolvable() string { return os.Getenv(envKnown) }

func viaParameter(key string) string {
	return os.Getenv(key)
}

func viaLoopVariable(names []string) []string {
	var out []string
	for _, name := range names {
		out = append(out, os.Getenv(name))
	}
	return out
}

func viaField(e struct{ Name string }) string {
	return os.Getenv(e.Name)
}

func viaComposedPrefix(prefix string) string {
	return os.Getenv(prefix + "SECRET")
}
`
	out, err := ScanReads(writeGoFixture(t, map[string]string{"component/x/a.go": src}))
	if err != nil {
		t.Fatalf("ScanReads: %v", err)
	}

	if got := keysOf(out); len(got) != 1 || got[0] != "MEMQL_FIXTURE_KNOWN" {
		t.Fatalf("reads = %v, want exactly [MEMQL_FIXTURE_KNOWN]: the four dynamic sites must not "+
			"invent a key, and the one constant site must still fold", got)
	}

	if len(out.Unresolvable) != 4 {
		t.Fatalf("residual = %d site(s), want 4.\nGot: %v\nZERO is the failure this test exists for: "+
			"the sites were dropped, the count looks clean, and nothing tells a reader that a "+
			"parameter-keyed read exists at all.", len(out.Unresolvable), out.Unresolvable)
	}

	// Every residual must be locatable -- an unresolvable count with no
	// addresses is a number a reader cannot act on.
	wantLines := map[int]string{
		10: "key",
		16: "name",
		22: "e.Name",
		26: `prefix + "SECRET"`,
	}
	for _, u := range out.Unresolvable {
		if u.File != "component/x/a.go" {
			t.Errorf("residual file = %q, want component/x/a.go", u.File)
		}
		arg, ok := wantLines[u.Line]
		if !ok {
			t.Errorf("residual at unexpected line %d: %s", u.Line, u)
			continue
		}
		if u.Arg != arg {
			t.Errorf("residual at line %d has Arg %q, want %q", u.Line, u.Arg, arg)
		}
		if strings.TrimSpace(u.Why) == "" {
			t.Errorf("residual %s carries no reason; a reader has to be told WHY it did not fold, "+
				"otherwise they cannot judge whether it is a knob to register or a helper that "+
				"only shares the env* name shape", u)
		}
		if u.Call == "" {
			t.Errorf("residual at line %d names no callee", u.Line)
		}
	}
}

// The residual has to reach the DRIFT surface, not just the scan. `-check` and
// TestNoEnvRegistryDrift read Result, so a residual that stops at Outcome would
// leave the reported line exactly as silent as before.
func TestCheckDriftCarriesTheResidual(t *testing.T) {
	root := newDriftFixture(t, []string{"MEMQL_FIXTURE_LIVE"}, []string{"MEMQL_FIXTURE_LIVE"})
	writeFixtureFile(t, root, "component/x/dynamic.go", `package x

import "os"

func viaParameter(key string) string { return os.Getenv(key) }
`)

	res, err := CheckDrift(root)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if len(res.Unresolvable) != 1 {
		t.Fatalf("Result.Unresolvable = %v, want 1 site: the residual must survive the trip from "+
			"Scan into Result or every consumer reports a total it cannot justify", res.Unresolvable)
	}
	// And it must NOT be drift: an unresolvable site is a gap in what the
	// check can see, not a var somebody failed to register. Failing on it
	// would make the only fix deleting the dynamic read.
	if !res.OK() {
		t.Errorf("a residual site turned the check red (unregistered=%v stale=%v); it is a "+
			"reporting obligation, not a violation", res.Unregistered, res.Stale)
	}
}

// An env.NewEnvReader read whose PREFIX IS KNOWN must produce the JOINED key
// (memql#3834).
//
// This test used to assert the opposite -- that every reader read is a residual
// -- and its own failure message said why the change is right: "under MEMQL_EDGE
// the first call reads MEMQL_EDGE_API_TARGET, so recording \"API_TARGET\" would
// register a name nothing sets." Recording the SUFFIX was always wrong. What was
// missing was the prefix, and when the constructor is right there in the same
// declaration with a foldable argument, the prefix is not missing at all.
//
// The suffix alone must still never become a read -- that is the confidently
// wrong answer this whole scanner exists to avoid, and it is asserted below.
func TestEnvReaderReadJoinsAKnownPrefix(t *testing.T) {
	const src = `package x

import "github.com/znasllc-io/memql/core/env"

func load() {
	reader := env.NewEnvReader("MEMQL_EDGE")
	_, _ = reader.String("API_TARGET")
	_, _ = reader.OptionalInt("SITE_CACHE_TTL_SECONDS")
	_, _ = reader.OptionalBool("ENABLED")
}
`
	root := writeGoFixture(t, map[string]string{
		"app/x.go": src,
		"go.mod":   "module github.com/znasllc-io/memql\n\ngo 1.26.1\n",
	})
	out, err := ScanReads(root)
	if err != nil {
		t.Fatalf("ScanReads: %v", err)
	}

	want := []string{"MEMQL_EDGE_API_TARGET", "MEMQL_EDGE_ENABLED", "MEMQL_EDGE_SITE_CACHE_TTL_SECONDS"}
	got := keysOf(out)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("reads = %v, want %v. The prefix is written in the same declaration and the "+
			"suffix at the call site; joining them is what turns two halves of a key into the "+
			"key the process will actually look up.", got, want)
	}
	for _, k := range got {
		if !strings.HasPrefix(k, "MEMQL_EDGE_") {
			t.Errorf("read %q is not under the constructor's prefix -- recording a bare suffix "+
				"registers a name nothing sets, which is worse than reporting a gap", k)
		}
	}
	if len(out.Unresolvable) != 0 {
		t.Errorf("residual = %v, want none: every prefix here folds, so nothing is unknown",
			out.Unresolvable)
	}
}

// A reader whose PREFIX DOES NOT FOLD is still a residual, with the reason
// naming the mechanism.
//
// This is the half of the old contract that must survive. The prefix here comes
// from a parameter, so the full key genuinely is not written down anywhere the
// scanner can see -- and guessing it (or recording the bare suffix) would be the
// confidently-wrong answer. Roughly half the reader sites in the real tree are
// still this shape.
func TestEnvReaderReadIsResidualWhenThePrefixIsUnknown(t *testing.T) {
	const src = `package x

import "github.com/znasllc-io/memql/core/env"

func load(prefix string) {
	reader := env.NewEnvReader(prefix)
	_, _ = reader.String("API_TARGET")
	_, _ = reader.OptionalBool("ENABLED")
}
`
	root := writeGoFixture(t, map[string]string{
		"app/x.go": src,
		"go.mod":   "module github.com/znasllc-io/memql\n\ngo 1.26.1\n",
	})
	out, err := ScanReads(root)
	if err != nil {
		t.Fatalf("ScanReads: %v", err)
	}

	if got := keysOf(out); len(got) != 0 {
		t.Errorf("reads = %v, want none. The prefix is a PARAMETER, so the full key is not "+
			"knowable here -- recording the suffix would register a name nothing sets.", got)
	}
	if len(out.Unresolvable) != 2 {
		t.Fatalf("residual = %d site(s), want 2.\nGot: %v\nZERO means the reader form is detected as "+
			"NOTHING -- neither read nor residual -- which is the class the summary line used to "+
			"omit while a reader had to find the limitation in a doc comment.",
			len(out.Unresolvable), out.Unresolvable)
	}
	for _, u := range out.Unresolvable {
		if !strings.Contains(u.Why, "env.NewEnvReader") {
			t.Errorf("residual %s does not name the mechanism, so a reader cannot tell it apart "+
				"from a parameter-keyed os.Getenv -- which needs a different fix", u)
		}
		if !strings.Contains(u.Why, "prefix") {
			t.Errorf("residual reason %q does not say the prefix is what is missing", u.Why)
		}
		if u.File != "app/x.go" || u.Line == 0 {
			t.Errorf("residual %s is not locatable", u)
		}
	}
}

// An EMPTY constructor prefix means the suffix IS the whole key.
//
// This is not an edge case -- 8 of the 32 constructor sites in this tree pass
// "", which is why their "suffixes" are already spelled as full MEMQL_ names.
// Eleven previously-invisible variables (the agent turn guards and the planner
// fairness/watchdog knobs) were exactly this shape.
func TestEnvReaderEmptyPrefixMakesTheSuffixTheKey(t *testing.T) {
	const src = `package x

import "github.com/znasllc-io/memql/core/env"

func load() {
	reader := env.NewEnvReader("")
	_, _ = reader.OptionalBool("MEMQL_TASK_FAIRNESS_ENABLED")
}
`
	root := writeGoFixture(t, map[string]string{
		"app/x.go": src,
		"go.mod":   "module github.com/znasllc-io/memql\n\ngo 1.26.1\n",
	})
	out, err := ScanReads(root)
	if err != nil {
		t.Fatalf("ScanReads: %v", err)
	}
	want := []string{"MEMQL_TASK_FAIRNESS_ENABLED"}
	if got := keysOf(out); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("reads = %v, want %v. An empty prefix contributes no separator, so the key is "+
			"the suffix verbatim -- and NOT joining it leaves a live operator knob invisible to "+
			"the registry.", got, want)
	}
}

// A reader REBOUND to a second prefix in the same declaration resolves to
// NEITHER.
//
// Every read after the rebind would be ambiguous and picking either binding
// would be a guess. Ambiguity is recorded as absence, which lands the sites in
// the residual -- the same stance the constant resolver takes for a name bound
// to two values across build tags.
func TestEnvReaderRebindingMakesThePrefixAmbiguous(t *testing.T) {
	const src = `package x

import "github.com/znasllc-io/memql/core/env"

func load(flag bool) {
	reader := env.NewEnvReader("MEMQL_A")
	reader = env.NewEnvReader("MEMQL_B")
	_, _ = reader.String("K")
}
`
	root := writeGoFixture(t, map[string]string{
		"app/x.go": src,
		"go.mod":   "module github.com/znasllc-io/memql\n\ngo 1.26.1\n",
	})
	out, err := ScanReads(root)
	if err != nil {
		t.Fatalf("ScanReads: %v", err)
	}
	if got := keysOf(out); len(got) != 0 {
		t.Errorf("reads = %v, want none. Two bindings means the key at this site is not "+
			"determined; MEMQL_A_K and MEMQL_B_K are both plausible and recording either is a "+
			"guess presented as a measurement.", got)
	}
	if len(out.Unresolvable) == 0 {
		t.Error("an ambiguous prefix produced no residual either, so the site vanished from " +
			"both the count and the gap report")
	}
}

// Every way a reader value reaches a call site must be detected, and detection
// must be anchored on the CONSTRUCTOR or the TYPE rather than the receiver's
// NAME.
//
// The final case is the negative one, and it is not hypothetical. Two non-test
// files hold a value named `reader`, reached as `X.reader.M(…)`, that reads no
// environment: component/metadata/geoip.go's `g.reader.City(ip)` (field
// `reader *geoip2.Reader`) and component/harness/reconciler.go's
// `r.reader.PlanStatus(ctx, …)` (field `reader StepReader`). A name-keyed rule
// counts both. Cited by path and expression rather than line, so a grep that
// returns nothing is how the citation goes stale -- loudly.
func TestEnvReaderReceiverShapes(t *testing.T) {
	cases := []struct {
		name          string
		src           string
		wantResiduals int
		// wantReads is how many KEYS the case resolves. Detection is the
		// property under test, and since memql#3834 a detected reader read
		// lands as a read when its constructor prefix folds and as a residual
		// when it does not -- so a case asserting zero of both is asserting the
		// receiver was not recognised at all.
		wantReads int
	}{
		{
			name: "local assigned from the constructor",
			src: `package x

import "github.com/znasllc-io/memql/core/env"

func load() { r := env.NewEnvReader("P"); _, _ = r.String("K") }
`,
			// Detected AND resolved: the prefix is a literal in the same
			// declaration, so the key is P_K.
			wantResiduals: 0,
			wantReads:     1,
		},
		{
			name: "parameter declared with the type",
			src: `package x

import "github.com/znasllc-io/memql/core/env"

func read(reader env.EnvReader, key string) (string, bool) { return reader.String(key) }
`,
			wantResiduals: 1,
		},
		{
			name: "struct field declared with the type",
			src: `package x

import "github.com/znasllc-io/memql/core/env"

type describer struct{ reader env.EnvReader }

func (d *describer) get() (string, bool) { return d.reader.String("HOST") }
`,
			wantResiduals: 1,
		},
		{
			name: "chained straight off the constructor",
			src: `package x

import "github.com/znasllc-io/memql/core/env"

func load() { _, _ = env.NewEnvReader("P").String("K") }
`,
			// The constructor is in the expression itself, so the prefix is
			// known without any binding to trace.
			wantResiduals: 0,
			wantReads:     1,
		},
		{
			// The negative case, and the reason the rule is type-anchored: a
			// field called `reader` of a DIFFERENT type, with a method whose
			// name happens to be in the EnvReader read set.
			name: "same-named field of a different type is not a reader",
			src: `package x

type geo struct{ reader *cityDB }

type cityDB struct{}

func (c *cityDB) String(s string) (string, bool) { return s, true }

func (g *geo) lookup() (string, bool) { return g.reader.String("NOT_AN_ENV_VAR") }
`,
			wantResiduals: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeGoFixture(t, map[string]string{
				"app/x.go": tc.src,
				"go.mod":   "module github.com/znasllc-io/memql\n\ngo 1.26.1\n",
			})
			out, err := ScanReads(root)
			if err != nil {
				t.Fatalf("ScanReads: %v", err)
			}
			readerSites := CountKind(out.Unresolvable, KindReaderPrefix)
			if readerSites != tc.wantResiduals {
				t.Errorf("reader residuals = %d, want %d (all residuals: %v)",
					readerSites, tc.wantResiduals, out.Unresolvable)
			}
			if got := keysOf(out); len(got) != tc.wantReads {
				t.Errorf("reads = %v (%d), want %d. A reader read resolves when its "+
					"constructor prefix folds and is a residual when it does not; zero of "+
					"BOTH means the receiver was not recognised as a reader at all, which is "+
					"what this test exists to catch.", got, len(got), tc.wantReads)
			}
		})
	}
}

// envReaderReadMethods is a closed set, so a method ADDED to core/env's reader
// would silently stop its call sites being counted -- a fresh blind spot opened
// by the mechanism whose whole job is to report them. This makes it a build
// failure instead.
//
// It PARSES core/env/reader.go rather than reflecting over it, so the scan
// package does not import the thing it measures.
func TestEnvReaderReadMethodsAreComplete(t *testing.T) {
	path := filepath.Join(repoRoot(t), "core", "env", "reader.go")
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	seen := map[string]bool{}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || !fn.Name.IsExported() {
			continue
		}
		onReader := false
		for _, field := range fn.Recv.List {
			if isEnvReaderType(field.Type) {
				onReader = true
			}
		}
		if !onReader || fn.Type.Params == nil || len(fn.Type.Params.List) == 0 {
			continue
		}
		// A key-taking method is one whose FIRST parameter is a string.
		// WithLookup's first parameter is a func, so it drops out here: it
		// configures a reader rather than reading a variable.
		first, ok := fn.Type.Params.List[0].Type.(*ast.Ident)
		if !ok || first.Name != "string" {
			continue
		}
		seen[fn.Name.Name] = true
		if !envReaderReadMethods[fn.Name.Name] {
			t.Errorf("core/env's EnvReader has an exported key-taking method %q that "+
				"envReaderReadMethods does not list. Every call to it is an env read this scanner "+
				"reports as NOTHING -- not a read, not a residual -- which is exactly the blind "+
				"spot the detector exists to close. Add it to the map.", fn.Name.Name)
		}
	}

	for name := range envReaderReadMethods {
		if !seen[name] {
			t.Errorf("envReaderReadMethods lists %q, which core/env's EnvReader no longer has as an "+
				"exported key-taking method. A stale entry means the detector matches a method name "+
				"that moved on; drop it.", name)
		}
	}
	if len(seen) == 0 {
		t.Fatal("found no key-taking EnvReader methods at all, so this guard asserted nothing -- " +
			"the parse or the receiver test is wrong, not core/env")
	}
}

// The reader class must be POPULATED on the real tree. A detector that compiles,
// passes its fixtures, and matches nothing in the corpus is the same false
// negative wearing a test suite.
func TestEnvReaderSitesAreCountedOnTheRealTree(t *testing.T) {
	out, err := ScanReads(repoRoot(t))
	if err != nil {
		t.Fatalf("ScanReads: %v", err)
	}
	readerSites := CountKind(out.Unresolvable, KindReaderPrefix)
	// A floor, not an exact number: these move whenever a config loader gains
	// a knob. Measured at 105 when the form landed; 40 is far below that and
	// far above anything a broken detector would produce.
	if readerSites < 40 {
		t.Errorf("only %d env.NewEnvReader site(s) counted on the real tree. Dozens of config "+
			"loaders are built on it, so a number this low means receiver detection stopped "+
			"matching and the class went back to being uncounted.", readerSites)
	}
	t.Logf("env.NewEnvReader sites counted: %d of %d residual sites", readerSites, len(out.Unresolvable))
}

// The pre-convention exemption is the one place this change WEAKENS the forward
// gate, so it gets the same treatment as the residual: reported, bounded, and
// falsifiable.
//
// withSyntheticExemption installs a one-key ownedPreConvention for the duration
// of a test and restores the real (empty) one afterwards.
//
// The production list is EMPTY since memql#3831 renamed all six of its members.
// Testing the mechanism against it would therefore assert nothing -- every
// arm below would pass over an empty loop. A synthetic key keeps the machinery
// covered for the day a seventh pre-convention name turns up, which is exactly
// when nobody would notice it had rotted. Same reasoning, and the same shape, as
// the synthetic baseline in scripts/ci/ruleset_drift_test.go.
func withSyntheticExemption(t *testing.T, key string) {
	t.Helper()
	prev := ownedPreConvention
	ownedPreConvention = map[string]bool{key: true}
	t.Cleanup(func() { ownedPreConvention = prev })
}

// Six memQL-owned keys predated the MEMQL_ prefix convention. Registering one
// failed component/genesis's TestOwnedVarsArePrefixed; omitting one failed
// forward drift. They are renamed now (memql#3831), but an exemption nobody
// reads is how a gap becomes permanent, so these tests still pin that an
// exemption is visible, that it is not laundered through `external`, and that
// every member is real.
func TestPreConventionExemptionIsReportedRatherThanSilent(t *testing.T) {
	withSyntheticExemption(t, "CACHE_MAX_TTL")

	root := newDriftFixture(t, []string{"MEMQL_FIXTURE_LIVE"}, []string{"MEMQL_FIXTURE_LIVE"})
	// A read of an exempt key, registered nowhere.
	writeFixtureFile(t, root, "component/x/legacy.go", `package x

import "os"

const envCacheMaxTTL = "CACHE_MAX_TTL"

func read() string { return os.Getenv(envCacheMaxTTL) }
`)

	res, err := CheckDrift(root)
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if len(res.ExemptUnprefixed) != 1 || res.ExemptUnprefixed[0] != "CACHE_MAX_TTL" {
		t.Fatalf("ExemptUnprefixed = %v, want [CACHE_MAX_TTL]. An empty list means the exemption is "+
			"invisible, which is the same failure as dropping an unresolvable site.", res.ExemptUnprefixed)
	}
	for _, k := range res.Unregistered {
		if k == "CACHE_MAX_TTL" {
			t.Error("CACHE_MAX_TTL was counted as forward drift as well as exempt")
		}
	}
	if !res.OK() {
		t.Errorf("exempt key turned the check red: unregistered=%v stale=%v", res.Unregistered, res.Stale)
	}
	// It is still a READ. The exemption is about what the registry must
	// name, not about whether the code reads it.
	out, err := ScanReads(root)
	if err != nil {
		t.Fatalf("ScanReads: %v", err)
	}
	found := false
	for _, k := range keysOf(out) {
		if k == "CACHE_MAX_TTL" {
			found = true
		}
	}
	if !found {
		t.Error("CACHE_MAX_TTL is missing from the reads. An exempt key must still be a read -- " +
			"hiding it there is what `external` does, and it is precisely what these keys are not.")
	}
}

// TestPreConventionExemptionIsEmpty is the memql#3831 result, asserted rather
// than assumed.
//
// The six names it held are renamed and registered, so there is nothing left to
// exempt -- and pinning that means re-adding an entry is a deliberate, failing
// act that forces whoever does it to re-read why the list exists. Left
// unasserted, "empty" is indistinguishable from "somebody quietly put a new
// unprefixed var back", which is the exact drift the list was created to make
// visible.
//
// If a seventh pre-convention name really is discovered, the fix is the same
// one that emptied this list: rename it, alias the old name, register the new.
// Not an entry here.
func TestPreConventionExemptionIsEmpty(t *testing.T) {
	if len(ownedPreConvention) == 0 {
		return
	}
	keys := make([]string, 0, len(ownedPreConvention))
	for k := range ownedPreConvention {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Errorf("ownedPreConvention has %d entr(y/ies): %v.\n"+
		"It was emptied by memql#3831, which renamed all six of its members to MEMQL_ "+
		"names with the old names recorded as legacy aliases. An exemption is not the "+
		"fix for an unprefixed var -- it is what made the prefix lint and the drift gate "+
		"each green via the other's blind spot. Rename it, alias the old name in "+
		"component/envregistry/legacyalias.go, and register the new one.", len(keys), keys)
}

// The exemption must not be laundered through the `external` denylist, which
// means "not memQL's variable" and exempts a key from BOTH drift directions.
//
// Vacuous while the list is empty, and deliberately kept: it is the property a
// seventh entry would have to satisfy, and the moment it is added is the moment
// nobody would think to write this test. It states its own coverage so the pass
// cannot be read as a claim about code it never examined.
func TestPreConventionKeysAreNotTreatedAsExternal(t *testing.T) {
	t.Logf("examined %d exemption(s)", len(ownedPreConvention))
	for key := range ownedPreConvention {
		if isExternal(key) {
			t.Errorf("%s is in the external denylist as well as ownedPreConvention. external means "+
				"the var is not memQL's; this one is read by memQL's own config loader, and being "+
				"external would drop it from the read set entirely.", key)
		}
	}
}

// Every member of the exemption must still be a live read on the real tree, and
// must still be absent from the registry.
//
// Without this the list is unfalsifiable: a name whose read is deleted, or which
// somebody later registers properly, would sit here forever suppressing a check
// that has nothing left to suppress. Both directions fail loudly instead.
//
// Vacuous while the list is empty (see TestPreConventionExemptionIsEmpty), and
// kept for the same reason as the test above it.
func TestPreConventionExemptionHasNoStaleMembers(t *testing.T) {
	t.Logf("examined %d exemption(s)", len(ownedPreConvention))
	root := repoRoot(t)
	out, err := ScanReads(root)
	if err != nil {
		t.Fatalf("ScanReads: %v", err)
	}
	read := map[string]bool{}
	for _, k := range keysOf(out) {
		read[k] = true
	}
	manifest, err := LoadRegistry(root)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	registered := RegisteredSet(manifest)

	for key := range ownedPreConvention {
		if !read[key] {
			t.Errorf("%s is exempted but no longer read anywhere. Delete the ownedPreConvention "+
				"entry -- an exemption for a var that does not exist suppresses nothing and "+
				"documents a knob operators cannot use.", key)
		}
		if registered[key] {
			t.Errorf("%s is BOTH registered and exempted from forward drift. The exemption exists "+
				"only because it could not be registered, so registering it makes the entry dead "+
				"code: delete it from ownedPreConvention.", key)
		}
	}
}

// A constant name bound to two DIFFERENT values in one package -- which happens
// legitimately across mutually-exclusive build tags -- must resolve to NEITHER.
//
// This is the one place where the constant-aware scan could produce a
// confidently wrong answer rather than no answer, and it is the caveat the
// earlier grep-based measurement of this issue explicitly could not rule out.
// Picking the first value would register a var nothing reads and leave the real
// one unregistered, with the gate green either way.
func TestConflictingConstantIsResidualRatherThanAGuess(t *testing.T) {
	root := writeGoFixture(t, map[string]string{
		"component/x/a_voice.go": `//go:build voice

package x

const envMode = "MEMQL_FIXTURE_VOICE_MODE"
`,
		"component/x/a_default.go": `//go:build !voice

package x

const envMode = "MEMQL_FIXTURE_DEFAULT_MODE"
`,
		"component/x/read.go": `package x

import "os"

func read() string { return os.Getenv(envMode) }
`,
	})

	out, err := ScanReads(root)
	if err != nil {
		t.Fatalf("ScanReads: %v", err)
	}
	if got := keysOf(out); len(got) != 0 {
		t.Errorf("reads = %v, want none: the name has two values under mutually exclusive build "+
			"tags, so ANY single answer is wrong and would be reported as fact", got)
	}
	if len(out.Unresolvable) != 1 {
		t.Fatalf("residual = %v, want 1 site naming the ambiguity", out.Unresolvable)
	}
	if !strings.Contains(out.Unresolvable[0].Why, "two different string values") {
		t.Errorf("residual reason %q does not explain the ambiguity", out.Unresolvable[0].Why)
	}
}

// A file the scanner cannot parse is a file whose env reads are invisible --
// this scanner's entire defect class. So it errors rather than skipping.
func TestUnparsableFileIsAnErrorNotASkip(t *testing.T) {
	root := writeGoFixture(t, map[string]string{
		"component/x/broken.go": "package x\n\nfunc read( { }\n",
	})
	_, err := ScanReads(root)
	if err == nil {
		t.Fatal("ScanReads succeeded on an unparsable file. Skipping it would make every read in that " +
			"file invisible while the count still looked clean -- the defect this scanner exists " +
			"to close, applied per-file.")
	}
	if !strings.Contains(err.Error(), "component/x/broken.go") {
		t.Errorf("error does not name the file it could not parse: %v", err)
	}
}

// The point of the whole change, measured on the real tree: the scanner must
// see MORE than the literal-only patterns did.
//
// A FLOOR rather than an exact number, so adding a var does not fail this test.
// What it defends is that the constant path stays wired: reverting the AST scan
// drops the count back to the literal-only 197 and this reds.
func TestReadCountExceedsLiteralOnlyBaseline(t *testing.T) {
	// literalOnlyBaseline is what `envscan -list | wc -l` reported on
	// origin/main at d97c63c0, with the three literal regexes.
	const literalOnlyBaseline = 197

	out, err := ScanReads(repoRoot(t))
	if err != nil {
		t.Fatalf("ScanReads: %v", err)
	}
	keys := keysOf(out)
	if len(keys) <= literalOnlyBaseline {
		t.Fatalf("scanner resolved %d keys, want more than the literal-only %d. Equal-or-fewer means "+
			"the constant path is not running, and registering names without it buys documentation "+
			"rather than a gate (memql#3818).", len(keys), literalOnlyBaseline)
	}

	// Two named witnesses, each read ONLY through a constant, so the count
	// above cannot be satisfied by some unrelated change in the corpus.
	witnesses := []string{
		"MEMQL_IDENTITY_BADGE_GRANT_PER_HOUR", // os.Getenv(envBadgeGrantPerHour)
		"MEMQL_OPERATOR_KEY",                  // os.Getenv(auth.EnvOperatorKey), cross-package
	}
	have := map[string]bool{}
	for _, k := range keys {
		have[k] = true
	}
	for _, w := range witnesses {
		if !have[w] {
			t.Errorf("%s is not among the resolved reads. It is read through a named constant and "+
				"nothing else, so its absence means that form is invisible again.", w)
		}
	}
	t.Logf("resolved %d keys (literal-only baseline %d), %d unresolvable site(s)",
		len(keys), literalOnlyBaseline, len(out.Unresolvable))
}

// PrintUnresolvable is what the CLI renders, so its line format is part of the
// contract: file:line first, so a reader can paste it into an editor.
func TestPrintUnresolvableRendersLocations(t *testing.T) {
	var b strings.Builder
	PrintUnresolvable(&b, []Unresolvable{
		{Call: "os.Getenv", Arg: "key", File: "component/x/a.go", Line: 11, Why: "key is not a string constant in scope"},
	})
	got := b.String()
	if !strings.HasPrefix(got, "component/x/a.go:11\t") {
		t.Errorf("rendered %q; want it to lead with file:line", got)
	}
	for _, want := range []string{"os.Getenv(key)", "not a string constant"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered %q, missing %q", got, want)
		}
	}
}

// Sanity: the scanner must not read its own source or _test.go files as env
// reads, which is what scannable() has always promised. Regressing that would
// register this file's fixture names.
func TestScanSkipsTestFilesAndItsOwnPackage(t *testing.T) {
	root := writeGoFixture(t, map[string]string{
		"component/x/a_test.go": `package x

import "os"

func read() string { return os.Getenv("MEMQL_FIXTURE_FROM_TEST") }
`,
		"cmd/envscan/scan/notes.go": `package scan

import "os"

func read() string { return os.Getenv("MEMQL_FIXTURE_FROM_SCANNER") }
`,
	})
	out, err := ScanReads(root)
	if err != nil {
		t.Fatalf("ScanReads: %v", err)
	}
	if got := keysOf(out); len(got) != 0 {
		t.Errorf("reads = %v, want none", got)
	}
	if len(out.Unresolvable) != 0 {
		t.Errorf("residual = %v, want none", out.Unresolvable)
	}
}

// modulePath has to survive a go.mod that is absent (fixtures) as well as one
// that is present, because cross-package resolution degrades differently in
// each case and a silent wrong answer there is the worst outcome.
func TestModulePathReadsGoMod(t *testing.T) {
	root := t.TempDir()
	if got := modulePath(root); got != "" {
		t.Errorf("modulePath with no go.mod = %q, want empty", got)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.com/thing\n\ngo 1.26.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := modulePath(root); got != "example.com/thing" {
		t.Errorf("modulePath = %q, want example.com/thing", got)
	}
}

// A residual's String() is what lands in the -check output; keep it printable
// even for an odd argument shape rather than panicking mid-report.
func TestExprTextHandlesUnusualArguments(t *testing.T) {
	root := writeGoFixture(t, map[string]string{"component/x/a.go": `package x

import "os"

func read(m map[string]string, p *string) (string, string, string) {
	return os.Getenv(m["k"]), os.Getenv(*p), os.Getenv(lookup())
}

func lookup() string { return "" }
`})
	out, err := ScanReads(root)
	if err != nil {
		t.Fatalf("ScanReads: %v", err)
	}
	if len(out.Unresolvable) != 3 {
		t.Fatalf("residual = %v, want 3", out.Unresolvable)
	}
	for _, u := range out.Unresolvable {
		if s := fmt.Sprint(u); strings.Contains(s, "%!") || strings.Contains(u.Arg, "<") && !strings.Contains(u.Arg, "(") {
			t.Errorf("residual renders unhelpfully: %q", s)
		}
	}
}
