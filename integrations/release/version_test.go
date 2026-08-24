package release

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
)

// version_test.go -- the arithmetic, and the one cross-language contract it
// has to hold.

func TestParseReleaseTagAcceptsOnlyReleaseTags(t *testing.T) {
	// The accept/reject table. Every reject is a shape that really appears
	// on a repository's tag list, and each is a different way a looser
	// parse goes wrong.
	for _, tc := range []struct {
		tag  string
		want bool
		why  string
	}{
		{"v1.2.3", true, "the canonical form"},
		{"v0.0.0", true, "zeroes are a version"},
		{"v10.20.30", true, "multi-digit components"},
		{"1.2.3", false, "no leading v -- stackCheckout could not find this tag"},
		{"v1.2", false, "two components is not a release"},
		{"v1.2.3.4", false, "four components is not a release"},
		{"v1.2.3-rc1", false, "a pre-release is not a release"},
		{"v1.2.3+build5", false, "a build suffix is not a release"},
		{"V1.2.3", false, "capital V is a different tag"},
		{"backup-v1.2.3", false, "an unanchored parse would read a backup ref as a release"},
		{"v1.2.3-old", false, "and would read a suffixed one too"},
		{"nightly", false, "a moving ref is not a release"},
		{"", false, "empty is not a tag"},
		{" v1.2.3", false, "leading space -- the caller trims, the parser does not guess"},
	} {
		if _, got := parseReleaseTag(tc.tag); got != tc.want {
			t.Errorf("parseReleaseTag(%q) = %v, want %v -- %s", tc.tag, got, tc.want, tc.why)
		}
	}
}

func TestNewestReleasePicksNumericallyNotLexicographically(t *testing.T) {
	// v0.9.2 sorts AFTER v0.17.0 as a string. A lexicographic max would
	// name a nine-month-old release as newest and cut the next version
	// from it, reissuing versions that already exist.
	got, ok := newestRelease([]string{"v0.9.2", "v0.17.0", "v0.10.5", "nightly", "v0.18.0-rc1"})
	if !ok || got.tag() != "v0.17.0" {
		t.Fatalf("newest = %v (%v), want v0.17.0", got.tag(), ok)
	}
	if _, ok := newestRelease([]string{"nightly", "latest", "v1.0-beta"}); ok {
		t.Fatal("a list with no release tag reported one")
	}
	if _, ok := newestRelease(nil); ok {
		t.Fatal("an empty list reported a release")
	}
}

func TestBumpZeroesTheLowerParts(t *testing.T) {
	v, _ := parseReleaseTag("v1.9.4")
	for _, tc := range []struct{ part, want string }{
		{"major", "v2.0.0"},
		{"minor", "v1.10.0"},
		{"patch", "v1.9.5"},
	} {
		got, err := v.bump(tc.part)
		if err != nil {
			t.Fatalf("bump %s: %v", tc.part, err)
		}
		if got.tag() != tc.want {
			t.Errorf("v1.9.4 %s -> %s, want %s", tc.part, got.tag(), tc.want)
		}
	}
	// No empty-string default, unlike deployversion's. Here the argument is
	// @required in the DSL, so an empty value means something went wrong
	// upstream and guessing "patch" would cut a release nobody asked for.
	if _, err := v.bump(""); err == nil {
		t.Fatal("an empty bump defaulted to something instead of refusing")
	}
	if got := RefusalCode(mustErr(v.bump("PATCH"))); got != CodeInvalidBump {
		t.Fatalf("a wrong-case bump gave %q, want %q", got, CodeInvalidBump)
	}
}

func mustErr(_ version, err error) error { return err }

func TestTagAndBareAreTheTwoConventions(t *testing.T) {
	// memql#4061: git tags carry the leading v, image tags do not. Both
	// spellings are load-bearing in opposite directions, so both are
	// rendered from one parsed value rather than string-munged at use
	// sites.
	v, _ := parseReleaseTag("v1.2.3")
	if v.tag() != "v1.2.3" || v.bare() != "1.2.3" {
		t.Fatalf("tag=%q bare=%q", v.tag(), v.bare())
	}
}

func TestNormalizeVersionTakesEitherSpelling(t *testing.T) {
	for _, in := range []string{"v1.2.3", "1.2.3"} {
		v, ok := normalizeVersion(in)
		if !ok || v.tag() != "v1.2.3" {
			t.Fatalf("normalizeVersion(%q) = %v (%v)", in, v.tag(), ok)
		}
	}
	for _, in := range []string{"vv1.2.3", "1.2", "rubbish", ""} {
		if _, ok := normalizeVersion(in); ok {
			t.Errorf("normalizeVersion(%q) accepted a non-version", in)
		}
	}
}

// TestParseRulesMatchTheExtension is the cross-language contract, and the one
// test in this file that reaches outside the package.
//
// WHY IT MATTERS. The extension's parseSemver decides which tags appear in the
// install picker; this package's decides which versions get cut. If they drift,
// the cutter creates releases the installer will not offer -- a failure with no
// error anywhere, discovered by a user who cannot find the version they were
// told to install. Neither side's own tests can see it: each would still be
// internally correct.
//
// It compares the PATTERNS rather than running the TypeScript, which is the
// strongest check available without a node runtime in this lane. The extension's
// regex is a one-line literal and this reads it directly, so a change on either
// side that is not matched on the other fails here.
//
// A CHECKER MUST REPORT ITS OWN COVERAGE. If the file cannot be read or the
// pattern cannot be located, this FAILS rather than skipping: a silent skip
// would make the contract look checked while checking nothing, which is worse
// than not having the test.
func TestParseRulesMatchTheExtension(t *testing.T) {
	const extensionFile = "../../editors/vscode/src/install/tags.ts"
	src, err := os.ReadFile(extensionFile)
	if err != nil {
		t.Fatalf("could not read %s: %v.\nThis test pins the cutter's tag rules to the installer's. "+
			"If the file MOVED, update the path here; do not delete the test -- the drift it "+
			"catches has no other detector.", extensionFile, err)
	}

	// The extension spells it as a JS regex literal inside parseSemver.
	// Matched as a LITERAL SUBSTRING rather than as a pattern: the thing
	// being located is itself a regex, and expressing it as one means
	// escaping every metacharacter twice, which is how a locator ends up
	// silently matching nothing.
	const extensionPattern = `/^v(\d+)\.(\d+)\.(\d+)$/`
	if !strings.Contains(string(src), extensionPattern) {
		t.Fatalf("could not find the pattern %s in %s.\n"+
			"Either parseSemver changed -- in which case this package's releaseTagRe must change with it, "+
			"or the cutter will create versions the installer refuses to offer -- or the literal was "+
			"reformatted and this locator needs updating.", extensionPattern, extensionFile)
	}

	// And the Go side must be the same rule. Compared as a normalized
	// string so the two are checked against each other rather than each
	// against a hand-copy of itself.
	goPattern := strings.NewReplacer("(", "", ")", "").Replace(releaseTagRe.String())
	if goPattern != `^v\d+\.\d+\.\d+$` {
		t.Fatalf("releaseTagRe is %q, which is not the extension's rule (^v\\d+\\.\\d+\\.\\d+$)", releaseTagRe.String())
	}

	// The behavioural half: the shapes both sides must agree about. A
	// pattern comparison alone would pass if one side trimmed its input
	// and the other did not.
	for _, tag := range []string{"v1.2.3", "1.2.3", "v1.2.3-rc1", "v1.2", "V1.2.3"} {
		_, goAccepts := parseReleaseTag(tag)
		// The extension's rule, spelled independently here so this is a
		// comparison and not a tautology.
		tsAccepts := regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`).MatchString(tag)
		if goAccepts != tsAccepts {
			t.Errorf("%q: the cutter says %v, the installer's rule says %v", tag, goAccepts, tsAccepts)
		}
	}
}

// TestParseRepoIsStrictAboutTheOwnerNameForm covers the config value an
// operator is most likely to paste a URL into.
func TestParseRepoIsStrictAboutTheOwnerNameForm(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"acme/widget", true},
		{"acme/widget/", true},
		{" acme/widget ", true},
		{"https://github.com/acme/widget", false},
		{"git@github.com:acme/widget", false},
		{"acme", false},
		{"acme/widget/extra", false},
		{"/widget", false},
		{"acme/", false},
		{"", false},
	} {
		_, got := parseRepo(tc.in)
		if got != tc.want {
			t.Errorf("parseRepo(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestResolverPrefersTheSecretOverTheVariable is a security property, not an
// implementation detail: the token is a credential, and a value found in the
// encrypted store must win over one sitting in plaintext config.
func TestResolverPrefersTheSecretOverTheVariable(t *testing.T) {
	r := resolver{
		systemSecret:   func(_ ctxT, name string) (string, error) { return "from-secret", nil },
		systemVariable: func(_ ctxT, name string) (string, error) { return "from-variable", nil },
		env:            func(string) string { return "from-env" },
	}
	if got := r.resolve(nil, SecretName); got != "from-secret" {
		t.Fatalf("resolve = %q, want the secret to win", got)
	}
}

func TestResolverFallsThroughAMISSANDANERROR(t *testing.T) {
	// A resolver ERROR must be treated exactly like a miss. On a node whose
	// database is still coming up the store is unreachable, and stopping
	// the walk there would report credential_unavailable for a token the
	// env has -- during exactly the window the env fallback exists for.
	r := resolver{
		systemSecret:   func(_ ctxT, _ string) (string, error) { return "", errBoom },
		systemVariable: func(_ ctxT, _ string) (string, error) { return "   ", nil },
		env:            func(string) string { return "from-env" },
	}
	if got := r.resolve(nil, SecretName); got != "from-env" {
		t.Fatalf("resolve = %q, want the env fallback", got)
	}
}

// ctxT keeps the resolver-table tests readable; the resolver never inspects the
// context, which is why nil is safe here and stated rather than assumed.
type ctxT = context.Context

var errBoom = errors.New("the store is unreachable")
