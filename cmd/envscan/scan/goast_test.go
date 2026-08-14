package scan

import (
	"fmt"
	"os"
	"path/filepath"
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

// The pre-convention exemption is the one place this change WEAKENS the forward
// gate, so it gets the same treatment as the residual: reported, bounded, and
// falsifiable.
//
// Six memQL-owned keys predate the MEMQL_ prefix convention. Registering one
// fails component/genesis's TestOwnedVarsArePrefixed; omitting one fails forward
// drift. Until they are renamed they are exempt -- and an exemption nobody reads
// is how a gap becomes permanent, so these tests pin that it is visible, that it
// is not laundered through `external`, and that every member is still real.
func TestPreConventionExemptionIsReportedRatherThanSilent(t *testing.T) {
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

// The exemption must not be laundered through the `external` denylist, which
// means "not memQL's variable" and exempts a key from BOTH drift directions.
func TestPreConventionKeysAreNotTreatedAsExternal(t *testing.T) {
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
func TestPreConventionExemptionHasNoStaleMembers(t *testing.T) {
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
