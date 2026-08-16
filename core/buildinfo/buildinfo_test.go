package buildinfo

import (
	"regexp"
	"testing"
)

// releaseShaped is the shape a version comparator accepts as a release tag:
// optional leading v, then major.minor.patch. It mirrors what
// editors/vscode/src/version/compare.ts parses (memql#3991) closely enough to
// answer the only question these tests ask -- "would a client believe this
// string names a release?" -- which is the property the whole package exists
// to get right.
var releaseShaped = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+$`)

// stampRelease sets the link-time variable for the duration of one test. Tests
// are the only place Go may write it; see the package comment.
func stampRelease(t *testing.T, value string) {
	t.Helper()
	prior := release
	release = value
	t.Cleanup(func() { release = prior })
}

// TestUnstampedBuildDoesNotNameARelease is THE regression this package exists
// to prevent (memql#3998).
//
// The failure it guards is not "the version is missing" -- it is "the version
// is present, release-shaped, and wrong", which is what the checked-in VERSION
// file produced for every build between v0.16.1 and this change. A client
// comparing `0.15.0` against the newest release gets a confident answer to a
// question it should have refused, and an operator on v0.18.0 is told they are
// three releases behind (or, worse, that they are current) on the strength of a
// number nobody set.
//
// A test build is by definition unstamped, so this asserts the default.
func TestUnstampedBuildDoesNotNameARelease(t *testing.T) {
	if got := Release(); got != "" {
		t.Fatalf("test binaries are not release builds; Release() = %q, want \"\"", got)
	}
	if IsRelease() {
		t.Fatal("IsRelease() = true on an unstamped build")
	}

	got := Version()
	if got != DevVersion {
		t.Fatalf("Version() = %q on an unstamped build, want %q", got, DevVersion)
	}
	if releaseShaped.MatchString(got) {
		t.Fatalf("Version() = %q on an unstamped build, which parses as a release tag -- "+
			"a build that was not cut from a release must not name one", got)
	}
}

// TestDevVersionIsNotReleaseShaped pins the constant itself. Replacing "dev"
// with something tidier-looking such as "0.0.0" would reintroduce the exact
// failure above while looking like a cosmetic change, because 0.0.0 parses and
// compares.
func TestDevVersionIsNotReleaseShaped(t *testing.T) {
	if DevVersion == "" {
		t.Fatal("DevVersion is empty; Version() must never return an empty string")
	}
	if releaseShaped.MatchString(DevVersion) {
		t.Fatalf("DevVersion = %q parses as a release tag; pick a value no release parser accepts", DevVersion)
	}
}

func TestStampedBuildStatesItsRelease(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stamp string
		want  string
	}{
		{name: "v-prefixed tag", stamp: "v0.18.1", want: "v0.18.1"},
		{name: "unprefixed tag", stamp: "0.18.1", want: "0.18.1"},
		// The build arg arrives through a shell and a Dockerfile; a trailing
		// newline or a stray space must not become part of the version.
		{name: "surrounding whitespace", stamp: "  v0.19.0\n", want: "v0.19.0"},
		// An explicitly empty -X (the build arg was declared but not set) is
		// the same as no stamp at all, not a version of "".
		{name: "whitespace only", stamp: "   ", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stampRelease(t, tc.stamp)

			if got := Release(); got != tc.want {
				t.Fatalf("Release() = %q, want %q", got, tc.want)
			}
			if got, want := IsRelease(), tc.want != ""; got != want {
				t.Fatalf("IsRelease() = %v, want %v", got, want)
			}

			wantVersion := tc.want
			if wantVersion == "" {
				wantVersion = DevVersion
			}
			if got := Version(); got != wantVersion {
				t.Fatalf("Version() = %q, want %q", got, wantVersion)
			}
		})
	}
}

// TestVersionIsNeverEmpty guards the one contract every caller relies on:
// Version is stamped into logs, health output, ServerHello.engine_version and
// the memqlVersion() builtin, none of which have a sensible rendering for "".
func TestVersionIsNeverEmpty(t *testing.T) {
	for _, stamp := range []string{"", " ", "\n", "v1.2.3"} {
		stampRelease(t, stamp)
		if Version() == "" {
			t.Fatalf("Version() = \"\" with release stamped as %q", stamp)
		}
	}
}
